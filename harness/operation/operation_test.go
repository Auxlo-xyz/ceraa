package operation

import (
	"bytes"
	"ceraa/harness/redact"
	"ceraa/internal/secret"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/harness/event"
	"ceraa/harness/question"
)

func TestShellOperation(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-1",
		Spec:   Spec{Type: "shell", Args: map[string]any{"command": "echo hello"}},
		Status: StatusPending,
	})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("expected completed, got %s: error=%q output=%q", r.Status, r.Error, r.Output)
		}
		if r.Output != "hello\n" {
			t.Fatalf("expected 'hello', got %q", r.Output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for result")
	}
}

func TestReadOperation(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-read",
		Spec:   Spec{Type: "read", Args: map[string]any{"path": "../../go.mod"}},
		Status: StatusPending,
	})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("expected completed, got %s: error=%q output=%q", r.Status, r.Error, r.Output[:min(len(r.Output), 50)])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

// The glob, grep, ls and catalog operation types were removed along with their
// tool schemas. They used to stay reachable by name alone, so a model that
// hallucinated a retired tool name still got real work done: a `ls` call returned
// a genuine directory listing. The operation layer and the tool layer have to
// agree on what exists, or the schema stops describing the surface.
func TestRemovedOperationTypesAreRejected(t *testing.T) {
	for _, spec := range []Spec{
		{Type: "glob", Args: map[string]any{"pattern": "go.mod"}},
		{Type: "grep", Args: map[string]any{"pattern": "func main"}},
		{Type: "ls", Args: map[string]any{"path": "."}},
		{Type: "skills_catalog", Args: map[string]any{}},
		{Type: "command_catalog", Args: map[string]any{}},
		{Type: "mcp_catalog", Args: map[string]any{}},
	} {
		mgr := New(nil)
		mgr.Add(Operation{ID: "t-" + spec.Type, Spec: spec, Status: StatusPending})
		select {
		case r := <-mgr.Results():
			if r.Status == StatusCompleted {
				t.Errorf("removed operation type %q still executed (output %q)", spec.Type, r.Output)
			}
			if r.Error == "" {
				t.Errorf("removed operation type %q failed without saying why", spec.Type)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: timeout", spec.Type)
		}
	}
}

// The types that remain must keep working; the removal above is only safe
// because these are untouched.
func TestRetainedOperationTypesStillRun(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-read",
		Spec:   Spec{Type: "read", Args: map[string]any{"path": "operation.go"}},
		Status: StatusPending,
	})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("read failed: %s %s", r.Status, r.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestCancelOperationRemovesFromManager(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-cancel",
		Spec:   Spec{Type: "shell", Args: map[string]any{"command": "echo done"}},
		Status: StatusPending,
	})
	time.Sleep(200 * time.Millisecond)
	mgr.Cancel("test-cancel")
	// cancel is async - just verify it doesn't panic
	select {
	case r := <-mgr.Results():
		// may complete normally or get canceled - either is acceptable
		_ = r
	default:
	}
	// Wait and verify no panic occurred
	time.Sleep(100 * time.Millisecond)
}

func TestQuestionAnswer(t *testing.T) {
	const sessionID = "test-question-session"
	const requestID = "question-1"
	events := event.Default.Subscribe(sessionID, 4)
	defer event.Default.Unsubscribe(sessionID, events)

	mgr := New(nil)
	mgr.Add(Operation{
		ID: requestID,
		Spec: Spec{Type: "question", Args: map[string]any{
			"question":   "Proceed?",
			"options":    []string{"Yes", "No"},
			"session_id": sessionID,
		}},
		Status: StatusPending,
	})

	select {
	case e := <-events:
		if e.Type != event.TypeQuestion {
			t.Fatalf("expected question event, got %s", e.Type)
		}
		if !question.Default.Answer(sessionID, requestID, "Yes") {
			t.Fatal("expected pending question answer to be accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("question event timeout")
	}

	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("expected completed, got %s: %s", r.Status, r.Error)
		}
		if r.Output != "Yes" {
			t.Fatalf("expected Yes, got %q", r.Output)
		}
	case <-time.After(time.Second):
		t.Fatal("question result timeout")
	}
}

func TestCancelStopsShellOperation(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-cancel-shell",
		Spec:   Spec{Type: "shell", Args: map[string]any{"command": "sleep 5"}},
		Status: StatusPending,
	})
	time.Sleep(100 * time.Millisecond)
	mgr.Cancel("test-cancel-shell")
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCanceled && r.Status != StatusFailed {
			t.Fatalf("expected canceled or failed, got %s", r.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shell operation did not stop after cancellation")
	}
}

func TestUnknownOperationType(t *testing.T) {
	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "test-unknown",
		Spec:   Spec{Type: "nonexistent", Args: map[string]any{}},
		Status: StatusPending,
	})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusFailed {
			t.Fatalf("expected failed, got %s", r.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > len(sub) && containsIn(s, sub)))
}

func containsIn(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Reading an image must attach it, not transcribe its bytes.
//
// The generic reader returns string(bytes) and caps at 5 MB, so a screenshot came
// back as a hundred thousand tokens of mojibake: the context budget was spent
// before the model saw anything, and nothing said so. The model is multimodal, so
// the picture goes over as an image and the text says so much.
func TestReadAnImageAttachesItInsteadOfDumpingBytes(t *testing.T) {
	dir := t.TempDir()
	shot := filepath.Join(dir, "shot.png")
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shot, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := New(nil)
	mgr.Add(Operation{
		ID:     "read-image",
		Spec:   Spec{Type: "read", Args: map[string]any{"path": shot}},
		Status: StatusPending,
	})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("status %s: %s", r.Status, r.Error)
		}
		if len(r.Images) != 1 || r.Images[0] != shot {
			t.Fatalf("images = %v, want the file it read", r.Images)
		}
		if !strings.Contains(r.Output, "attached as an image") {
			t.Errorf("output = %q, want it to say the image was attached", r.Output)
		}
		// The decisive assertion: no binary in the text.
		if strings.Contains(r.Output, "PNG") || strings.Contains(r.Output, "\x89") {
			t.Errorf("image bytes reached the result text: %q", r.Output[:min(len(r.Output), 120)])
		}
		if len(r.Output) > 300 {
			t.Errorf("result text is %d bytes for a 64x64 image; the bytes are still going in as text", len(r.Output))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
	}
}

// A non-image must be unaffected, or this fix breaks ordinary reads.
func TestReadATextFileIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := New(nil)
	mgr.Add(Operation{ID: "read-text", Spec: Spec{Type: "read", Args: map[string]any{"path": path}}, Status: StatusPending})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("status %s: %s", r.Status, r.Error)
		}
		if r.Output != "hello\nworld\n" {
			t.Errorf("output = %q", r.Output)
		}
		if len(r.Images) != 0 {
			t.Errorf("a text file produced images: %v", r.Images)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// The reason this tool exists. Five separate edits cost five round trips and five
// chances to leave a file half-changed; a set that does not match must write
// nothing at all.
func TestBatchEditIsAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	original := "alpha\nbeta\ngamma\ndelta\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// The third edit does not exist in the file.
	mgr := New(nil)
	mgr.Add(Operation{
		ID: "batch-bad",
		Spec: Spec{Type: "batch_edit_file", Args: map[string]any{
			"path": path,
			"edits": []map[string]any{
				{"old_text": "alpha", "new_text": "ALPHA"},
				{"old_text": "gamma", "new_text": "GAMMA"},
				{"old_text": "epsilon", "new_text": "EPSILON"},
			},
		}},
		Status: StatusPending,
	})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusFailed {
			t.Fatalf("a set with a bad edit reported %s", r.Status)
		}
		if !strings.Contains(r.Error, "edit 2") {
			t.Errorf("error = %q, want it to name which edit did not match", r.Error)
		}
		if !strings.Contains(r.Error, "found 0") {
			t.Errorf("error = %q, want the occurrence count", r.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatalf("the file was written despite a failed set:\n%q", after)
	}
}

// Two of the three matching is still a failure, and a duplicated match is a
// failure: an ambiguous replacement is not a replacement.
func TestBatchEditRejectsAmbiguousAndEmptyEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	original := "dup\ndup\nonly\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, edits := range map[string][]map[string]any{
		"ambiguous": {{"old_text": "dup", "new_text": "DUP"}},
		"empty":     {},
	} {
		mgr := New(nil)
		mgr.Add(Operation{ID: "batch-" + name, Spec: Spec{Type: "batch_edit_file", Args: map[string]any{"path": path, "edits": edits}}, Status: StatusPending})
		select {
		case r := <-mgr.Results():
			if r.Status != StatusFailed {
				t.Errorf("%s: status %s, want failed", name, r.Status)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: timeout", name)
		}
		if after, _ := os.ReadFile(path); string(after) != original {
			t.Errorf("%s: the file changed: %q", name, after)
		}
	}
}

// The good path: every change lands, in order, in one write.
func TestBatchEditAppliesEveryChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := New(nil)
	mgr.Add(Operation{ID: "batch-ok", Spec: Spec{Type: "batch_edit_file", Args: map[string]any{
		"path": path,
		"edits": []map[string]any{
			{"old_text": "one", "new_text": "1"},
			{"old_text": "two", "new_text": "2"},
			{"old_text": "three", "new_text": "3"},
		},
	}}, Status: StatusPending})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("status %s: %s", r.Status, r.Error)
		}
		if !strings.Contains(r.Output, "applied 3") {
			t.Errorf("output = %q, want it to report the count", r.Output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}
	after, _ := os.ReadFile(path)
	if string(after) != "1\n2\n3\n" {
		t.Errorf("file = %q", after)
	}
}

// A spec that has been through JSON arrives as []any, and asserting one shape
// silently yields no edits — the tool appears to do nothing.
func TestBatchEditAcceptsBothSpecShapes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x\ny\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := New(nil)
	mgr.Add(Operation{ID: "batch-json", Spec: Spec{Type: "batch_edit_file", Args: map[string]any{
		"path": path,
		"edits": []any{
			map[string]any{"old_text": "x", "new_text": "X"},
			map[string]any{"old_text": "y", "new_text": "Y"},
		},
	}}, Status: StatusPending})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("status %s: %s", r.Status, r.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}
	if after, _ := os.ReadFile(path); string(after) != "X\nY\n" {
		t.Errorf("file = %q", after)
	}
}

// The renamed tool still runs, and so does the type stored by sessions from
// before the rename.
func TestEditLineAndTheLegacyEditTypeBothRun(t *testing.T) {
	for _, specType := range []string{"edit_line", "edit"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mgr := New(nil)
		mgr.Add(Operation{ID: "e-" + specType, Spec: Spec{Type: specType, Args: map[string]any{
			"path": path, "old_text": "old", "new_text": "new",
		}}, Status: StatusPending})
		select {
		case r := <-mgr.Results():
			if r.Status != StatusCompleted {
				t.Fatalf("%s: status %s: %s", specType, r.Status, r.Error)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: timeout", specType)
		}
		if after, _ := os.ReadFile(path); string(after) != "new\n" {
			t.Errorf("%s: file = %q", specType, after)
		}
	}
}

// The guarantee: a secret is readable and usable, and never readable back.
//
// A value placed in a spec would be persisted as SpecArgs and reloaded on every
// later request in the session, so the spec carries the name and the value exists
// only inside the operation.
func TestASecretValueNeverEntersASpecOrAResult(t *testing.T) {
	openTestSecretStore(t)
	const value = "sk-live-DONTLEAKTHISVALUE1234567890"
	if err := secret.Set("probe_key", value); err != nil {
		t.Fatal(err)
	}
	redact.ResetLiterals()
	t.Cleanup(redact.ResetLiterals)
	SetProviderCredentialWriter(func(provider, got string) error {
		if got != value {
			return fmt.Errorf("the writer received %q, want the stored value", got)
		}
		return nil
	})
	t.Cleanup(func() { SetProviderCredentialWriter(nil) })

	// The spec, which is what gets persisted.
	spec := Spec{Type: "set_provider_key", Args: map[string]any{"provider": "custom", "secret": "probe_key"}}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), value) {
		t.Fatalf("the value is in the spec that gets persisted: %s", encoded)
	}

	mgr := New(nil)
	mgr.Add(Operation{ID: "spk", Spec: spec, Status: StatusPending})
	var out string
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("status %s: %s", r.Status, r.Error)
		}
		out = r.Output + r.Error
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}
	if strings.Contains(out, value) {
		t.Errorf("the value reached the result the model reads: %q", out)
	}
	if !strings.Contains(out, "chars") {
		t.Errorf("the result should describe the credential without showing it: %q", out)
	}
}

// The name of a secret is fine; Ceraa's own credentials are not. A tool that could
// read `ceraa_minimax_global` hands the model the account.
func TestReservedSecretsAreRefusedByName(t *testing.T) {
	openTestSecretStore(t)
	for _, name := range []string{"ceraa_minimax_global", "codex_access_token", "ceraa_xai_xai"} {
		if err := secret.Set(name, "internal-credential-value"); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ceraa_minimax_global", "codex_access_token"} {
		_, err := resolveUsableSecret(name)
		if err == nil {
			t.Errorf("%s was readable by a model-facing tool", name)
		} else if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%s: error = %q, want it to say reserved", name, err)
		}
	}
	if _, err := resolveUsableSecret("no_such_secret"); err == nil {
		t.Error("an unknown name was accepted")
	}
}

// The whole reason a command may be handed a secret at all: the value goes into
// the child process, and what the child prints is scrubbed on the way out. Without
// that, `env` puts it in the model's context.
func TestASecretHandedToACommandNeverComesBack(t *testing.T) {
	openTestSecretStore(t)
	const value = "plaintextsecret-not-pattern-shaped"
	if err := secret.Set("probe_env", value); err != nil {
		t.Fatal(err)
	}
	redact.ResetLiterals()
	t.Cleanup(redact.ResetLiterals)
	if _, err := resolveUsableSecret("probe_env"); err != nil {
		t.Fatal(err)
	}

	mgr := New(nil)
	mgr.Add(Operation{ID: "rcs", Spec: Spec{Type: "run_command_with_secret", Args: map[string]any{
		// The two commands a model reaches for to exfiltrate one.
		"command": "echo \"value=$PROBE\"; env | grep PROBE; printenv PROBE",
		"secret":  "probe_env",
		"env_var": "PROBE",
	}}, Status: StatusPending})
	select {
	case r := <-mgr.Results():
		if r.Status != StatusCompleted {
			t.Fatalf("status %s: %s", r.Status, r.Error)
		}
		if strings.Contains(r.Output, value) {
			t.Errorf("the command's own output carried the value into the result: %q", r.Output)
		}
		if !strings.Contains(r.Output, "redacted") {
			t.Errorf("expected a redaction marker in %q", r.Output)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timeout")
	}
}

// The value must not be in the command line, or `ps` publishes it.
func TestTheValueIsNotInTheCommandLine(t *testing.T) {
	openTestSecretStore(t)
	if err := secret.Set("probe_ps", "commandline-secret-value-9876"); err != nil {
		t.Fatal(err)
	}
	mgr := New(nil)
	mgr.Add(Operation{ID: "rcs-ps", Spec: Spec{Type: "run_command_with_secret", Args: map[string]any{
		"command": "cat /proc/$$/cmdline | tr '\\0' ' '",
		"secret":  "probe_ps",
		"env_var": "PROBE_PS",
	}}, Status: StatusPending})
	select {
	case r := <-mgr.Results():
		if strings.Contains(r.Output, "commandline-secret-value-9876") {
			t.Errorf("the value was on the command line: %q", r.Output)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timeout")
	}
}

func openTestSecretStore(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
}
