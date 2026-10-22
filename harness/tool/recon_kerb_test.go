package tool

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/harness/act"
	"ceraa/harness/recon"
)

// fakeKDC answers with one canned Kerberos reply.
type fakeKDC struct {
	listener net.Listener
}

func startFakeKDC(t *testing.T, reply []byte) *fakeKDC {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &fakeKDC{listener: listener}
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
	return server
}

func (s *fakeKDC) address() string {
	return s.listener.Addr().String()
}

// krbError is a KRB-ERROR with an error code and no text, written by hand.
func krbError(code int) []byte {
	body := []byte{0xa0, 0x03, 0x02, 0x01, 0x05, 0xa6, 0x03, 0x02, 0x01, byte(code)}
	out := []byte{0x6e, byte(len(body))}
	return append(out, body...)
}

func TestTheKerbToolNeedsAKDCAndARealm(t *testing.T) {
	tool := &KerbTool{}
	if _, err := tool.Translate(map[string]any{}); err == nil {
		t.Fatal("a call with no kdc was accepted")
	}
	if _, err := tool.Translate(map[string]any{"kdc": "kdc.example.com"}); err == nil {
		t.Fatal("a call with no realm was accepted")
	}
}

func TestTheKerbToolRefusesAListOverTheBound(t *testing.T) {
	tool := &KerbTool{}
	accounts := make([]any, kerbMaxAccounts+1)
	for i := range accounts {
		accounts[i] = "svc"
	}
	if _, err := tool.Translate(map[string]any{"kdc": "kdc.example.com", "realm": "EXAMPLE.COM", "accounts": accounts}); err == nil {
		t.Error("too many accounts were accepted")
	}
	passwords := make([]any, kerbMaxPasswords+1)
	for i := range passwords {
		passwords[i] = "x"
	}
	if _, err := tool.Translate(map[string]any{"kdc": "kdc.example.com", "realm": "EXAMPLE.COM", "passwords": passwords}); err == nil {
		t.Error("too many passwords were accepted")
	}
}

func TestTheKerbToolRefusesAResultNameThatIsAPath(t *testing.T) {
	tool := &KerbTool{}
	for _, name := range []string{"../escape.txt", "sub/dir.txt", "..", "."} {
		_, err := tool.Translate(map[string]any{"kdc": "kdc.example.com", "realm": "EXAMPLE.COM", "out": name})
		if err == nil {
			t.Errorf("the result name %q was accepted", name)
		}
	}
}

func TestTheKerbToolNeverPrintsThePassword(t *testing.T) {
	results := []act.RoastResult{{
		Account:   "svc_backup",
		Password:  "correct horse battery staple",
		Validated: true,
	}}
	output := formatKerbResults(results, "kdc.example.com:88", "EXAMPLE.COM", 3, "/tmp/roast.txt")
	if strings.Contains(output, "correct horse") {
		t.Errorf("the output carries the password:\n%s", output)
	}
	if !strings.Contains(output, "svc_backup") {
		t.Errorf("the output does not name the account:\n%s", output)
	}
	if !strings.Contains(output, "/tmp/roast.txt") {
		t.Errorf("the output does not name the result file:\n%s", output)
	}
	if !strings.Contains(output, "3") {
		t.Errorf("the output does not report the candidate count:\n%s", output)
	}
}

func TestTheKerbToolWritesTheResultFileAtSixHundred(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	results := []act.RoastResult{{
		Account:   "svc_backup",
		Password:  "correct horse battery staple",
		Validated: true,
	}}
	path, err := kerbWriteResults("roast.txt", results)
	if err != nil {
		t.Fatalf("write the results: %v", err)
	}
	if !strings.HasPrefix(path, filepath.Join(os.Getenv("CERAA_HOME"), "recon", "kerb")) {
		t.Errorf("the result file is not under the data directory: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the result file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the result file is mode %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the result file: %v", err)
	}
	if !strings.Contains(string(data), "correct horse") {
		t.Errorf("the result file does not carry the validated password:\n%s", data)
	}
}

func TestTheKerbToolReportsTheKDCRefusalWithoutAPassword(t *testing.T) {
	openTheTestEngagement(t)
	kdc := startFakeKDC(t, krbError(25))
	tool := &KerbTool{}
	spec, err := tool.Translate(map[string]any{
		"kdc":       kdc.address(),
		"realm":     "EXAMPLE.COM",
		"accounts":  []any{"svc_backup"},
		"passwords": []any{"correct horse battery staple"},
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	output, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(output, "correct horse") {
		t.Errorf("the output carries the password:\n%s", output)
	}
	if !strings.Contains(output, "svc_backup") {
		t.Errorf("the output does not name the account:\n%s", output)
	}
	if !strings.Contains(output, "preauthentication required") {
		t.Errorf("the output does not read the refusal as words:\n%s", output)
	}
}

func TestTheKerbToolRefusesAKDCTheEngagementDoesNotName(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	recon.Stop()
	t.Cleanup(func() { recon.Stop() })
	tool := &KerbTool{}
	spec, err := tool.Translate(map[string]any{
		"kdc":       "kdc.example.com",
		"realm":     "EXAMPLE.COM",
		"accounts":  []any{"svc_backup"},
		"passwords": []any{"x"},
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if _, err := tool.RunContext(context.Background(), spec); err == nil {
		t.Fatal("a KDC the engagement does not name was contacted")
	}
}

func TestTheKerbToolIsRegisteredUnderItsOperationType(t *testing.T) {
	tool := &KerbTool{}
	if tool.Name() != "kerb" {
		t.Errorf("the name is %q, want kerb", tool.Name())
	}
	spec, err := tool.Translate(map[string]any{"kdc": "kdc.example.com", "realm": "EXAMPLE.COM"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Type != tool.Name() {
		t.Errorf("the operation type is %q and the name is %q, and the registry matches them by name", spec.Type, tool.Name())
	}
	_ = time.Second
}
