// Package oast is the out-of-band address the recon tools hand to a target.
//
// A blind bug has no response to read: the injection fires somewhere the
// tester cannot see, and the only evidence is a request that comes back to an
// address the tester controls. This package mints that address and reads what
// arrived there.
//
// The wire protocol is interactsh's, because the public servers are the only
// free infrastructure that answers on DNS, HTTP and SMTP without an account.
// The client is deliberately small: one RSA key, one correlation id, one
// registration and one poll loop, all with the standard library. Pulling the
// projectdiscovery client in would bring its whole dependency tree for a
// hundred lines of protocol.
//
// Three properties matter more than the code:
//
//   - The address is per payload, not per run. Two findings must never share
//     a callback host, or a hit cannot be attributed to the finding that
//     caused it.
//   - A missing interaction is not a refutation. The package returns what
//     arrived and says nothing about what did not; the caller decides that a
//     window with no hit is inconclusive rather than false.
//   - The private key lives on disk, because the alternative is losing every
//     pending callback when the process restarts mid-engagement.
package oast

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ceraa/internal/logger"
	"ceraa/internal/paths"
)

// Marker is the placeholder the tools substitute. It expands to a bare host,
// so a payload that needs a URL writes http://{{oast}}/ and a payload that
// needs a hostname writes {{oast}} directly.
const Marker = "{{oast}}"

const (
	// The public servers accept a correlation id of 20 characters and slice
	// every subdomain label into windows of correlation+nonce length. Both
	// numbers are the interactsh defaults; a different length is not invalid,
	// but there is no reason to disagree with the server about the shape of
	// its own addresses.
	correlationLen = 20
	nonceLen       = 13
	// zbase32 is the alphabet interactsh uses for the nonce. Encryption does
	// not depend on it; interoperability with anything that parses these
	// hosts does.
	zbase32 = "ybndrfg8ejkmcpqxot1uwisza345h769"
	// pollInterval is how often a waiting caller asks. One second is short
	// enough that a fast HTTP callback lands inside a ten second budget and
	// long enough that a slow one does not cost a request per tick.
	pollInterval = 750 * time.Millisecond
	// registerEvery bounds how often a session is re-registered. The server
	// evicts an idle correlation id, and a probe run can stay idle for
	// minutes between a payload and the poll that checks it.
	registerEvery = 45 * time.Second
)

// Servers are tried in order until one accepts a registration. They are public
// and interchangeable; the first that answers is the one the session keeps,
// because a host minted against one server is not pollable at another.
var Servers = []string{
	"https://oast.pro",
	"https://oast.live",
	"https://oast.online",
	"https://oast.fun",
}

// baseDir is the directory the session lives in. A variable so tests aim it at
// a temp directory instead of the operator's data directory.
var baseDir = filepath.Join(paths.DataDir(), "oast")

// httpClient is the only way this package reaches the network. Tests replace
// it to drive a local interactsh-shaped server.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// session is the persisted state: where to poll, who we are there, and the key
// that decrypts what comes back.
type session struct {
	Server        string    `json:"server"`
	CorrelationID string    `json:"correlation_id"`
	SecretKey     string    `json:"secret_key"`
	PrivateKey    string    `json:"private_key"`
	RegisteredAt  time.Time `json:"registered_at"`
}

// Payload is one minted callback address.
type Payload struct {
	// Host is the bare hostname to inject.
	Host string
	// URL is http://Host, for payloads that need a URL.
	URL string
	// Server is the interactsh server that will answer for this host.
	Server string
}

// Interaction is one request that came back to a minted address.
type Interaction struct {
	Protocol      string    `json:"protocol"`
	UniqueID      string    `json:"unique-id"`
	FullID        string    `json:"full-id"`
	QType         string    `json:"q-type,omitempty"`
	RawRequest    string    `json:"raw-request,omitempty"`
	RemoteAddress string    `json:"remote-address"`
	Timestamp     time.Time `json:"timestamp"`
}

var (
	mu       sync.Mutex
	cached   *session
	cachedPK *rsa.PrivateKey
)

// Mint creates one callback host. The caller substitutes it into the request
// it is about to send.
func Mint(ctx context.Context) (Payload, error) {
	mu.Lock()
	defer mu.Unlock()

	s, _, err := ensureLocked(ctx)
	if err != nil {
		return Payload{}, err
	}

	nonce, err := randomString(nonceLen, zbase32)
	if err != nil {
		return Payload{}, err
	}
	host := strings.ToLower(s.CorrelationID + nonce + "." + serverHost(s.Server))
	return Payload{Host: host, URL: "http://" + host, Server: s.Server}, nil
}

// Await polls until an interaction for one of the hosts arrives or the budget
// runs out. It returns what it saw, not what it expected: an empty slice with
// a nil error means the poll worked and nothing came back, which the caller
// must not read as "the bug is absent".
func Await(ctx context.Context, hosts []string, budget time.Duration) ([]Interaction, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	deadline := time.Now().Add(budget)
	var found []Interaction
	for {
		var batch []Interaction
		if err := poll(ctx, &batch); err != nil {
			return found, err
		}
		for _, interaction := range batch {
			if matches(interaction, hosts) {
				found = append(found, interaction)
			}
		}
		if len(found) > 0 || time.Now().After(deadline) {
			return found, nil
		}
		select {
		case <-ctx.Done():
			return found, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Ensure makes sure a session exists and is recently registered. The tools call
// it before a poll so a session evicted while idle comes back rather than
// reporting an empty poll as a result.
func Ensure(ctx context.Context) error {
	mu.Lock()
	defer mu.Unlock()
	_, _, err := ensureLocked(ctx)
	return err
}

// Server reports the server the current session polls, for display.
func Server() string {
	mu.Lock()
	defer mu.Unlock()
	if cached != nil {
		return cached.Server
	}
	return ""
}

func ensureLocked(ctx context.Context) (*session, *rsa.PrivateKey, error) {
	if cached != nil && cachedPK != nil {
		if time.Since(cached.RegisteredAt) < registerEvery {
			return cached, cachedPK, nil
		}
		if err := register(ctx, cached); err == nil {
			cached.RegisteredAt = time.Now()
			_ = save(cached)
			return cached, cachedPK, nil
		}
		// A failed refresh is not a new session: the old key can still read
		// anything the server has not evicted.
		logger.Warn("oast: session refresh failed, keeping the current one")
		return cached, cachedPK, nil
	}

	s, key, err := load()
	if err == nil {
		cached, cachedPK = s, key
		if err := register(ctx, s); err == nil {
			s.RegisteredAt = time.Now()
			_ = save(s)
		}
		return cached, cachedPK, nil
	}

	s, key, err = mint(ctx)
	if err != nil {
		return nil, nil, err
	}
	cached, cachedPK = s, key
	if err := save(s); err != nil {
		// The session still works in this process; it just will not survive a
		// restart, and a pending callback would be lost with it.
		logger.Warn(fmt.Sprintf("oast: session not persisted: %s", err))
	}
	return cached, cachedPK, nil
}

// mint creates a session against the first server that accepts it.
func mint(ctx context.Context) (*session, *rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	correlation, err := randomString(correlationLen, "abcdefghijklmnopqrstuvwxyz0123456789")
	if err != nil {
		return nil, nil, err
	}
	secret, err := randomString(32, "abcdefghijklmnopqrstuvwxyz0123456789")
	if err != nil {
		return nil, nil, err
	}

	var lastErr error
	for _, server := range Servers {
		s := &session{
			Server:        strings.TrimRight(server, "/"),
			CorrelationID: correlation,
			SecretKey:     secret,
			PrivateKey:    encodePrivateKey(key),
		}
		if err := register(ctx, s); err != nil {
			lastErr = err
			continue
		}
		s.RegisteredAt = time.Now()
		return s, key, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no server configured")
	}
	return nil, nil, fmt.Errorf("register with every oast server failed: %w", lastErr)
}

// register announces the public key for the correlation id. The server answers
// 200 for a fresh registration and an error for an id it already knows, which
// is not a failure here: an already-live session is exactly what a refresh
// wants.
func register(ctx context.Context, s *session) error {
	pub, err := decodePrivateKey(s.PrivateKey)
	if err != nil {
		return err
	}
	encoded, err := encodePublicKey(&pub.PublicKey)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"public-key":     encoded,
		"secret-key":     s.SecretKey,
		"correlation-id": s.CorrelationID,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Server+"/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	// Some servers answer 400 with "already registered"; that is the state we
	// asked for. Anything else is a real refusal.
	return fmt.Errorf("%s answered %d", s.Server, resp.StatusCode)
}

// pollResponse is the server's answer. aes_key is the session's AES key,
// encrypted to the RSA key the client registered; each data entry is an
// interaction encrypted with that AES key.
type pollResponse struct {
	Data    []string `json:"data"`
	AESKey  string   `json:"aes_key"`
	TLDData []string `json:"tlddata"`
	Extra   []string `json:"extra"`
}

func poll(ctx context.Context, out *[]Interaction) error {
	mu.Lock()
	s, key, err := ensureLocked(ctx)
	mu.Unlock()
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("%s/poll?id=%s&secret=%s", s.Server, s.CorrelationID, s.SecretKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("poll %s: %w", s.Server, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode < 500 {
			// A server that forgot the session is recoverable: register again
			// and let the next poll find it. Anything the server already
			// dropped is gone, and the caller sees an empty poll rather than
			// an invented result.
			mu.Lock()
			if cached != nil {
				_ = register(ctx, cached)
				cached.RegisteredAt = time.Now()
				_ = save(cached)
			}
			mu.Unlock()
			return nil
		}
		return fmt.Errorf("poll %s: %d %s", s.Server, resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	var decoded pollResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&decoded); err != nil {
		return fmt.Errorf("poll %s: %w", s.Server, err)
	}

	for _, entry := range decoded.Data {
		plain, err := decrypt(key, decoded.AESKey, entry)
		if err != nil {
			logger.Warn(fmt.Sprintf("oast: interaction not decrypted: %s", err))
			continue
		}
		var interaction Interaction
		if err := json.Unmarshal(plain, &interaction); err != nil {
			logger.Warn(fmt.Sprintf("oast: interaction not decoded: %s", err))
			continue
		}
		*out = append(*out, boundInteraction(interaction))
	}
	for _, entry := range append(decoded.Extra, decoded.TLDData...) {
		var interaction Interaction
		if err := json.Unmarshal([]byte(entry), &interaction); err != nil {
			continue
		}
		*out = append(*out, boundInteraction(interaction))
	}
	return nil
}

// boundInteraction caps what the evidence can carry. The raw request is the
// proof, but a proof that quotes a whole POST body is a receipt nobody reads
// and a redaction problem nobody audited.
func boundInteraction(i Interaction) Interaction {
	i.RawRequest = clip(i.RawRequest, 1200)
	i.Protocol = clip(i.Protocol, 32)
	i.RemoteAddress = clip(i.RemoteAddress, 128)
	return i
}

func matches(i Interaction, hosts []string) bool {
	haystack := strings.ToLower(i.FullID + " " + i.UniqueID)
	for _, host := range hosts {
		label := strings.ToLower(strings.SplitN(strings.TrimSpace(host), ".", 2)[0])
		if len(label) >= correlationLen && strings.Contains(haystack, label) {
			return true
		}
	}
	return false
}

// decrypt reverses the server's envelope: RSA-OAEP for the AES key, then
// AES-CTR with the IV in the first block of the payload.
func decrypt(key *rsa.PrivateKey, encryptedKey, payload string) ([]byte, error) {
	rawKey, err := base64.StdEncoding.DecodeString(encryptedKey)
	if err != nil {
		return nil, err
	}
	aesKey, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, rawKey, nil)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < aes.BlockSize {
		return nil, fmt.Errorf("ciphertext shorter than its IV")
	}
	plain := make([]byte, len(ciphertext)-aes.BlockSize)
	cipher.NewCTR(block, ciphertext[:aes.BlockSize]).XORKeyStream(plain, ciphertext[aes.BlockSize:])
	return bytes.TrimRight(plain, " \t\r\n"), nil
}

func sessionPath() string { return filepath.Join(baseDir, "session.json") }

func load() (*session, *rsa.PrivateKey, error) {
	data, err := os.ReadFile(sessionPath())
	if err != nil {
		return nil, nil, err
	}
	var s session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, nil, err
	}
	if s.Server == "" || s.CorrelationID == "" || s.SecretKey == "" {
		return nil, nil, fmt.Errorf("session is incomplete")
	}
	key, err := decodePrivateKey(s.PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	return &s, key, nil
}

func save(s *session) error {
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := sessionPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, sessionPath())
}

func encodePrivateKey(key *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func decodePrivateKey(data string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(data))
	if block == nil {
		return nil, fmt.Errorf("private key is not PEM")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// encodePublicKey matches interactsh: PKIX DER inside a PEM block it labels
// "RSA PUBLIC KEY", base64 of the whole PEM. The label is a misnomer the
// servers parse as PKIX, so copying it is what keeps the registration readable
// on the other side.
func encodePublicKey(key *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", err
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: der})
	return base64.StdEncoding.EncodeToString(block), nil
}

func serverHost(server string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(server, "https://"), "http://")
	return strings.TrimRight(host, "/")
}

func randomString(n int, alphabet string) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(out), nil
}

func clip(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
