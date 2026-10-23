package oast

import (
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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is interactsh's register/poll contract with none of its storage:
// one RSA key per correlation id, and a queue of interactions the test puts
// there directly.
type fakeServer struct {
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	queues  map[string][]Interaction
	refused bool
}

func (f *fakeServer) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.refused {
			http.Error(w, "no", http.StatusBadRequest)
			return
		}
		var body struct {
			PublicKey     string `json:"public-key"`
			SecretKey     string `json:"secret-key"`
			CorrelationID string `json:"correlation-id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		raw, err := base64.StdEncoding.DecodeString(body.PublicKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		block, _ := pem.Decode(raw)
		if block == nil {
			http.Error(w, "not pem", http.StatusBadRequest)
			return
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		key, ok := parsed.(*rsa.PublicKey)
		if !ok {
			http.Error(w, "not rsa", http.StatusBadRequest)
			return
		}
		f.keys[body.CorrelationID] = key
		_, _ = w.Write([]byte(`{"message":"registration successful"}`))
	})
	mux.HandleFunc("/poll", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.URL.Query().Get("id")
		key, ok := f.keys[id]
		if !ok {
			http.Error(w, "could not find correlation id", http.StatusBadRequest)
			return
		}
		pending := f.queues[id]
		f.queues[id] = nil
		if len(pending) == 0 {
			_, _ = w.Write([]byte(`{"data":[],"aes_key":""}`))
			return
		}
		aesKey := make([]byte, 32)
		_, _ = rand.Read(aesKey)
		encryptedKey, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, key, aesKey, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data := make([]string, 0, len(pending))
		for _, interaction := range pending {
			plain, err := json.Marshal(interaction)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			block, _ := aes.NewCipher(aesKey)
			iv := make([]byte, aes.BlockSize)
			_, _ = rand.Read(iv)
			out := make([]byte, aes.BlockSize+len(plain))
			copy(out, iv)
			cipher.NewCTR(block, iv).XORKeyStream(out[aes.BlockSize:], plain)
			data = append(data, base64.StdEncoding.EncodeToString(out))
		}
		_ = json.NewEncoder(w).Encode(pollResponse{
			Data:   data,
			AESKey: base64.StdEncoding.EncodeToString(encryptedKey),
		})
	})
	return mux
}

func newFake(t *testing.T) (*fakeServer, *httptest.Server) {
	t.Helper()
	fake := &fakeServer{keys: map[string]*rsa.PublicKey{}, queues: map[string][]Interaction{}}
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	return fake, server
}

// isolate resets the package to a temp data dir pointing at the given server.
// The package caches a session and holds a client, so a test that does not do
// this inherits the previous test's state.
func isolate(t *testing.T, serverURL string) {
	t.Helper()
	baseDir = t.TempDir()
	Servers = []string{serverURL}
	httpClient = &http.Client{Timeout: 5 * time.Second}
	mu.Lock()
	cached, cachedPK = nil, nil
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		cached, cachedPK = nil, nil
		mu.Unlock()
	})
}

func TestMintPayloadShape(t *testing.T) {
	_, server := newFake(t)
	isolate(t, server.URL)

	payload, err := Mint(context.Background())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	label, host, ok := strings.Cut(payload.Host, ".")
	if !ok {
		t.Fatalf("host %q has no dot", payload.Host)
	}
	if len(label) != correlationLen+nonceLen {
		t.Errorf("label is %d chars, want %d: %q", len(label), correlationLen+nonceLen, label)
	}
	if host != strings.TrimPrefix(server.URL, "http://") {
		t.Errorf("host %q does not end in the poll server", payload.Host)
	}
	if payload.URL != "http://"+payload.Host {
		t.Errorf("url %q is not the host with a scheme", payload.URL)
	}
	if strings.ContainsAny(payload.Host, "{}") {
		t.Errorf("marker survived substitution: %q", payload.Host)
	}
}

func TestMintReusesSessionAndChangesPayload(t *testing.T) {
	_, server := newFake(t)
	isolate(t, server.URL)

	first, err := Mint(context.Background())
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	second, err := Mint(context.Background())
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if first.Host == second.Host {
		t.Error("two payloads share one host; a hit could not be attributed")
	}
	prefix := correlationLen
	if first.Host[:prefix] != second.Host[:prefix] {
		t.Errorf("payloads from one session have different correlation ids: %q vs %q", first.Host, second.Host)
	}
	if _, err := readSession(); err != nil {
		t.Errorf("session was not persisted: %v", err)
	}
}

func TestAwaitFindsOnlyItsOwnInteraction(t *testing.T) {
	fake, server := newFake(t)
	isolate(t, server.URL)

	mine, err := Mint(context.Background())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	other, err := Mint(context.Background())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	fake.mu.Lock()
	fake.queues[correlationOf(mine.Host)] = []Interaction{
		{Protocol: "http", UniqueID: strings.SplitN(other.Host, ".", 2)[0], FullID: other.Host, RemoteAddress: "203.0.113.9:1234", RawRequest: "GET /other HTTP/1.1"},
		{Protocol: "http", UniqueID: strings.SplitN(mine.Host, ".", 2)[0], FullID: mine.Host, RemoteAddress: "198.51.100.7:443", RawRequest: "GET /ssrf?u=http://x HTTP/1.1\r\nHost: x"},
	}
	fake.mu.Unlock()

	found, err := Await(context.Background(), []string{mine.Host}, 3*time.Second)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d interactions, want 1", len(found))
	}
	if found[0].RemoteAddress != "198.51.100.7:443" {
		t.Errorf("attributed the wrong interaction: %+v", found[0])
	}
	if !strings.Contains(found[0].RawRequest, "/ssrf") {
		t.Errorf("raw request was not decrypted: %q", found[0].RawRequest)
	}
}

func TestAwaitTimesOutWithNoInteraction(t *testing.T) {
	_, server := newFake(t)
	isolate(t, server.URL)

	payload, err := Mint(context.Background())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	start := time.Now()
	found, err := Await(context.Background(), []string{payload.Host}, 250*time.Millisecond)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found %d interactions on an empty queue", len(found))
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("a 250ms budget took %s", elapsed)
	}
}

func TestMintFallsBackWhenTheFirstServerRefuses(t *testing.T) {
	fake, server := newFake(t)
	isolate(t, server.URL)
	fake.refused = true

	if _, err := Mint(context.Background()); err == nil {
		t.Fatal("mint succeeded against a refusing server")
	}
	fake.mu.Lock()
	fake.refused = false
	fake.mu.Unlock()
	if _, err := Mint(context.Background()); err != nil {
		t.Fatalf("mint after the server recovered: %v", err)
	}
}

func readSession() (*session, error) {
	s, _, err := load()
	return s, err
}

func correlationOf(host string) string {
	return strings.SplitN(host, ".", 2)[0][:correlationLen]
}
