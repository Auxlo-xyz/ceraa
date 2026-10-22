package act

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"ceraa/harness/recon"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// krbRepFixture builds a KRB-REP for the test.
//
// The values are written out by hand so a reader can see the wire format:
// the pvno and the message type, the padata with the etype-info2, the realm,
// the client name, the ticket and the encrypted part. The framing is done
// with the codec, which is pinned separately against literal bytes.
func krbRepFixture(realm string, client string, etype int, cipher []byte, leakedKey []byte) []byte {
	// PA-ETYPE-INFO2: the rc4-hmac entry with the account's key in the
	// salt, which is the leak a roast lives on.
	salt := EncodeElement(Tag(0x1b), leakedKey)
	entry := EncodeElement(TagSequence, append(
		EncodeElement(Tag(0xa0), []byte{0x02, 0x01, 0x12}), // [0] etype 18
		EncodeElement(Tag(0xa1), salt)...,
	))
	// The etype-info2 is a SEQUENCE OF entries.
	etypeInfo2 := EncodeElement(TagSequence, entry)
	padata := EncodeElement(TagSequence, append(
		EncodeElement(Tag(0xa1), []byte{0x02, 0x01, 0x13}), // [1] padata-type 19
		EncodeElement(Tag(0xa2), EncodeElement(TagOctetString, etypeInfo2))...,
	))

	// The client name: a PrincipalName with a name type and one string.
	// The name string is a GeneralString, which is what KerberosString is
	// in RFC 4120.
	name := EncodeElement(TagSequence, append(
		EncodeElement(Tag(0xa0), []byte{0x02, 0x01, 0x01}), // [0] name-type 1
		EncodeElement(Tag(0xa1), EncodeElement(TagSequence, EncodeElement(Tag(0x1b), []byte(client))))...,
	))

	// The ticket is not read, so an empty sequence stands in for it.
	ticket := EncodeElement(TagSequence, nil)

	// The encrypted part: an EncryptedData with the type and the cipher.
	encPart := EncodeElement(TagSequence, append(
		EncodeElement(Tag(0xa0), []byte{0x02, 0x01, byte(etype)}),
		EncodeElement(Tag(0xa2), EncodeElement(TagOctetString, cipher))...,
	))

	body := append(
		EncodeElement(Tag(0xa0), []byte{0x02, 0x01, 0x05}),    // [0] pvno
		EncodeElement(Tag(0xa1), []byte{0x02, 0x01, 0x0c})..., // [1] msg-type 12
	)
	body = append(body, EncodeElement(Tag(0xa2), EncodeElement(TagSequence, padata))...)
	body = append(body, EncodeElement(Tag(0xa3), EncodeElement(Tag(0x1b), []byte(realm)))...)
	body = append(body, EncodeElement(Tag(0xa4), name)...)
	body = append(body, EncodeElement(Tag(0xa5), ticket)...)
	body = append(body, EncodeElement(Tag(0xa6), encPart)...)
	return EncodeElement(Tag(tagKrbRep), body)
}

// krbErrorFixture builds a KRB-ERROR for the test.
func krbErrorFixture(code int, text string) []byte {
	body := append(
		EncodeElement(Tag(0xa0), []byte{0x02, 0x01, 0x05}),    // [0] pvno
		EncodeElement(Tag(0xa1), []byte{0x02, 0x01, 0x1e})..., // [1] msg-type 30
	)
	body = append(body, EncodeElement(Tag(0xa6), []byte{0x02, 0x01, byte(code)})...)
	body = append(body, EncodeElement(Tag(0xab), EncodeElement(TagOctetString, []byte(text)))...)
	return EncodeElement(Tag(tagKrbError), body)
}

// replyPartPayload builds the plaintext of an encrypted part: the reply key
// and the checksum the KDC wrote over it.
func replyPartPayload(t *testing.T, replyKey []byte) []byte {
	t.Helper()
	checksum := truncate(hmacMD5(replyKey, krb5ChecksumConstant), rc4ChecksumLength)
	return append(
		EncodeElement(Tag(0x80), replyKey),
		EncodeElement(Tag(0x84), checksum)...,
	)
}

// encryptReplyPart encrypts a reply part the way a KDC does: the confounder
// with the first half of the key, the payload with the second.
func encryptReplyPart(t *testing.T, password string, payload []byte) []byte {
	t.Helper()
	key := rc4KeyFromPassword(password)
	confounder := make([]byte, rc4ConfounderLength)
	if _, err := rand.Read(confounder); err != nil {
		t.Fatalf("read the confounder: %v", err)
	}
	first := rc4Init(key[:rc4KeyLength/2])
	head := first.xor(confounder)
	first.skip(rc4KeyLength - rc4ConfounderLength)
	second := rc4Init(key[rc4KeyLength/2:])
	return append(head, second.xor(payload)...)
}

// startFakeKDC answers with one canned reply.
func startFakeKDC(t *testing.T, reply []byte) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var header [4]byte
				if _, err := io.ReadFull(conn, header[:]); err != nil {
					return
				}
				length := binary.BigEndian.Uint32(header[:])
				body := make([]byte, length)
				if _, err := io.ReadFull(conn, body); err != nil {
					return
				}
				framed := make([]byte, 4+len(reply))
				binary.BigEndian.PutUint32(framed[:4], uint32(len(reply)))
				copy(framed[4:], reply)
				conn.Write(framed)
			}(conn)
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return listener.Addr().String()
}

// ---------------------------------------------------------------------------
// The request
// ---------------------------------------------------------------------------

func TestTheASReqIsBuiltForTheAccount(t *testing.T) {
	encoded, err := buildASReq("EXAMPLE.COM", "svc_backup", 0x11223344)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	if encoded[0] != tagASReq {
		t.Fatalf("the request starts with 0x%02x, want 0x%02x", encoded[0], tagASReq)
	}
	// The pvno and the message type.
	if !strings.Contains(string(encoded), string([]byte{0x02, 0x01, 0x05})) {
		t.Error("the request does not carry the pvno")
	}
	if !strings.Contains(string(encoded), string([]byte{0x02, 0x01, 0x0a})) {
		t.Error("the request does not carry the AS-REQ message type")
	}
	// The account, the realm and the rc4-hmac type.
	if !strings.Contains(string(encoded), "svc_backup") {
		t.Error("the request does not name the account")
	}
	if !strings.Contains(string(encoded), "EXAMPLE.COM") {
		t.Error("the request does not carry the realm")
	}
	if !strings.Contains(string(encoded), string([]byte{0x02, 0x01, 0x12})) {
		t.Error("the request does not offer rc4-hmac")
	}
	// The PA-FOR-USER padata, type 2.
	if !strings.Contains(string(encoded), string([]byte{0x02, 0x01, 0x02})) {
		t.Error("the request does not carry a PA-FOR-USER padata")
	}
}

func TestTheASReqNeverCarriesThePreAuthentication(t *testing.T) {
	encoded, err := buildASReq("EXAMPLE.COM", "svc_backup", 1)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	// PA-PREAUTH (type 16) and PA-ETYPE-INFO (type 11) are the pre
	// authentication padata, and a roast must not send either.
	for _, forbidden := range [][]byte{{0x02, 0x01, 0x10}, {0x02, 0x01, 0x0b}} {
		if strings.Contains(string(encoded), string(forbidden)) {
			t.Errorf("the request carries the pre-authentication padata % x", forbidden)
		}
	}
}

// ---------------------------------------------------------------------------
// The reply
// ---------------------------------------------------------------------------

func TestTheASRepIsParsed(t *testing.T) {
	setEngagement(t, recon.Engagement{
		InScope:      []string{"127.0.0.1"},
		AllowPrivate: []string{"127.0.0.1"},
	})
	replyKey := make([]byte, rc4KeyLength)
	for i := range replyKey {
		replyKey[i] = byte(i + 1)
	}
	leaked := make([]byte, rc4KeyLength)
	for i := range leaked {
		leaked[i] = byte(0x40 + i)
	}
	cipher := encryptReplyPart(t, "correct horse", replyPartPayload(t, replyKey))
	address := startFakeKDC(t, krbRepFixture("EXAMPLE.COM", "svc_backup", etypeRC4HMAC, cipher, leaked))

	captured, err := RequestASRep(context.Background(), address, "EXAMPLE.COM", "svc_backup", 5*time.Second)
	if err != nil {
		t.Fatalf("the request failed: %v", err)
	}
	if captured.Realm != "EXAMPLE.COM" {
		t.Errorf("the realm is %q", captured.Realm)
	}
	if captured.Client != "svc_backup" {
		t.Errorf("the client is %q", captured.Client)
	}
	if captured.EType != etypeRC4HMAC {
		t.Errorf("the encryption type is %d, want %d", captured.EType, etypeRC4HMAC)
	}
	if len(captured.Cipher) != len(cipher) {
		t.Errorf("the cipher is %d bytes, want %d", len(captured.Cipher), len(cipher))
	}
	if len(captured.Key) != rc4KeyLength {
		t.Fatalf("the leaked key is %d bytes, want %d", len(captured.Key), rc4KeyLength)
	}
	for i := range leaked {
		if captured.Key[i] != leaked[i] {
			t.Fatalf("the leaked key was not read")
		}
	}
}

func TestTheKDCsRefusalIsReportedInWords(t *testing.T) {
	setEngagement(t, recon.Engagement{
		InScope:      []string{"127.0.0.1"},
		AllowPrivate: []string{"127.0.0.1"},
	})
	address := startFakeKDC(t, krbErrorFixture(25, "Additional pre-authentication required"))
	_, err := RequestASRep(context.Background(), address, "EXAMPLE.COM", "svc_backup", 5*time.Second)
	if err == nil {
		t.Fatal("the refusal was reported as a success")
	}
	if !strings.Contains(err.Error(), "preauthentication required") {
		t.Errorf("the refusal does not read as words: %v", err)
	}
	if !strings.Contains(err.Error(), "Additional pre-authentication required") {
		t.Errorf("the refusal dropped the KDC's own words: %v", err)
	}
}

func TestTheKDCIsRefusedWithoutAnEngagement(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	recon.Stop()
	address := startFakeKDC(t, krbErrorFixture(25, ""))
	_, err := RequestASRep(context.Background(), address, "EXAMPLE.COM", "svc_backup", 5*time.Second)
	if err == nil {
		t.Fatal("a KDC was asked with no engagement on record")
	}
	if !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("the refusal is not the authorization error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The validation
// ---------------------------------------------------------------------------

func TestTheRightPasswordValidatesAndTheWrongOneDoesNot(t *testing.T) {
	replyKey := make([]byte, rc4KeyLength)
	for i := range replyKey {
		replyKey[i] = byte(i + 1)
	}
	cipher := encryptReplyPart(t, "correct horse", replyPartPayload(t, replyKey))
	captured := ASRep{
		Realm:  "EXAMPLE.COM",
		Client: "svc_backup",
		EType:  etypeRC4HMAC,
		Cipher: cipher,
	}
	ok, err := Validate(captured, "correct horse")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !ok {
		t.Error("the right password did not validate")
	}
	ok, err = Validate(captured, "wrong horse")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if ok {
		t.Error("the wrong password validated")
	}
}

func TestTheLeakedKeyValidatesTheCandidateThatHashesToIt(t *testing.T) {
	leaked := rc4KeyFromPassword("correct horse")
	captured := ASRep{
		Realm:  "EXAMPLE.COM",
		Client: "svc_backup",
		EType:  etypeRC4HMAC,
		Cipher: []byte("not needed: the KDC leaked the key"),
		Key:    leaked,
	}
	ok, err := Validate(captured, "correct horse")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !ok {
		t.Error("the candidate that hashes to the leaked key did not validate")
	}
	ok, err = Validate(captured, "wrong horse")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if ok {
		t.Error("a candidate that does not hash to the leaked key validated")
	}
}

func TestValidationOfAnEmptyCaptureIsAnError(t *testing.T) {
	_, err := Validate(ASRep{}, "anything")
	if err == nil {
		t.Fatal("a capture with no encrypted part was validated")
	}
}

// ---------------------------------------------------------------------------
// The roast
// ---------------------------------------------------------------------------

func TestTheRoastIsBounded(t *testing.T) {
	accounts := make([]string, MaxRoastAccounts+1)
	for i := range accounts {
		accounts[i] = "svc"
	}
	if _, err := Roast(context.Background(), "127.0.0.1:88", "EXAMPLE.COM", accounts, []string{"x"}, time.Second); err == nil {
		t.Error("too many accounts were accepted")
	}
	passwords := make([]string, MaxRoastPasswords+1)
	for i := range passwords {
		passwords[i] = "x"
	}
	if _, err := Roast(context.Background(), "127.0.0.1:88", "EXAMPLE.COM", []string{"svc"}, passwords, time.Second); err == nil {
		t.Error("too many passwords were accepted")
	}
}

func TestTheRoastValidatesTheAccountThatAnswers(t *testing.T) {
	setEngagement(t, recon.Engagement{
		InScope:      []string{"127.0.0.1"},
		AllowPrivate: []string{"127.0.0.1"},
	})
	replyKey := make([]byte, rc4KeyLength)
	for i := range replyKey {
		replyKey[i] = byte(i + 1)
	}
	cipher := encryptReplyPart(t, "correct horse", replyPartPayload(t, replyKey))
	address := startFakeKDC(t, krbRepFixture("EXAMPLE.COM", "svc_backup", etypeRC4HMAC, cipher, nil))

	results, err := Roast(context.Background(), address, "EXAMPLE.COM", []string{"svc_backup"}, []string{"wrong horse", "correct horse"}, 5*time.Second)
	if err != nil {
		t.Fatalf("the roast failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("%d results, want 1", len(results))
	}
	if !results[0].Validated {
		t.Fatalf("the account was not validated: %+v", results[0])
	}
	if results[0].Password != "correct horse" {
		t.Errorf("the result carries %q", results[0].Password)
	}
	if results[0].Error != "" {
		t.Errorf("the result carries an error: %q", results[0].Error)
	}
}

func TestTheRoastReportsTheAccountTheKDCRefuses(t *testing.T) {
	setEngagement(t, recon.Engagement{
		InScope:      []string{"127.0.0.1"},
		AllowPrivate: []string{"127.0.0.1"},
	})
	address := startFakeKDC(t, krbErrorFixture(7, "Principal unknown"))
	results, err := Roast(context.Background(), address, "EXAMPLE.COM", []string{"nobody"}, []string{"x"}, 5*time.Second)
	if err != nil {
		t.Fatalf("the roast failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("%d results, want 1", len(results))
	}
	if results[0].Validated {
		t.Error("an account the KDC refused was validated")
	}
	if !strings.Contains(results[0].Error, "principal unknown") {
		t.Errorf("the error does not read as words: %q", results[0].Error)
	}
}
