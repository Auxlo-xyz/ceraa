package freebuff_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/freebuff"
	"ceraa/internal/secret"
	"path/filepath"
)

// TestHostedCompletionsRecoversFromSessionGate drives a real request through
// Ceraa's LLM transport: the hosted metadata must be on the wire, a
// session-ending gate must trigger exactly one re-admission, and the retry must
// succeed without the user seeing the gate.
func TestHostedCompletionsRecoversFromSessionGate(t *testing.T) {
	root := t.TempDir()
	if err := secret.Init(filepath.Join(root, "key"), filepath.Join(root, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FREEBUFF_CONFIG_DIR", t.TempDir())

	var admissions atomic.Int32
	var completions atomic.Int32
	var sawMetadata atomic.Value
	var sawUserAgent atomic.Value

	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	defer server.Close()

	mux.HandleFunc("/api/v1/freebuff/session/admission", func(w http.ResponseWriter, r *http.Request) {
		admissions.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     "active",
			"instanceId": "i1",
			"model":      r.Header.Get("x-freebuff-model"),
			"freebucks":  map[string]any{"balance": 100},
		})
	})
	mux.HandleFunc("/api/v1/freebuff/session", func(w http.ResponseWriter, r *http.Request) {
		// Before the first admission there is no row, so the probe-before-commit
		// path is exercised; afterwards the row reads back as active.
		if admissions.Load() == 0 {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "none"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "active", "instanceId": "i1", "model": "deepseek/deepseek-v4-flash",
			"freebucks": map[string]any{"balance": 100},
		})
	})
	mux.HandleFunc("/api/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		attempt := completions.Add(1)
		sawUserAgent.Store(r.Header.Get("user-agent"))
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		sawMetadata.Store(body["codebuff_metadata"])
		if attempt == 1 {
			// The row is gone; the reference recovers by re-admitting.
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "session_expired"})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })

	client := freebuff.NewClient()
	client.APIBaseURL = server.URL
	client.WebBaseURL = server.URL
	client.HTTPClient = server.Client()

	manager := freebuff.NewSessionManager(client)
	if _, err := manager.Ensure(context.Background(), "token", "deepseek/deepseek-v4-flash"); err != nil {
		t.Fatal(err)
	}
	if admissions.Load() != 1 {
		t.Fatalf("admissions = %d, want 1 before the request", admissions.Load())
	}

	cfg := llm.FromProvider(server.URL+"/api/v1", "token", "deepseek/deepseek-v4-flash")
	cfg.HTTPClient = server.Client()
	metadata := freebuff.NewMetadataBuilder()
	cfg.ExtraBodyFunc = func() map[string]any { return metadata.Body(manager.InstanceID()) }
	cfg.ExtraHeaders = metadata.Headers()
	cfg.RecoverError = freebuff.MakeRecoverError(manager)

	streamer := llm.New(cfg)
	var content string
	err := streamer.Stream(context.Background(), llm.StreamRequest{
		Messages: []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "hi"}},
	}, func(delta llm.ProviderResponse) {
		content += delta.Content
	})
	if err != nil {
		t.Fatalf("stream returned an error after a recoverable gate: %v", err)
	}
	if content != "hello" {
		t.Fatalf("content = %q", content)
	}
	if completions.Load() != 2 {
		t.Fatalf("completions attempts = %d, want 2", completions.Load())
	}
	if admissions.Load() < 2 {
		t.Fatalf("admissions = %d, want a re-admission after the gate", admissions.Load())
	}

	block, ok := sawMetadata.Load().(map[string]any)
	if !ok {
		t.Fatal("codebuff_metadata was not sent in the request body")
	}
	if block["cost_mode"] != "free" {
		t.Fatalf("cost_mode = %v, want free", block["cost_mode"])
	}
	if block["freebuff_instance_id"] == "" {
		t.Fatal("freebuff_instance_id was not sent")
	}
	ua, _ := sawUserAgent.Load().(string)
	if ua == "" {
		t.Fatal("user-agent was not sent")
	}
}

// TestWithdrawnModelIsNotRetried pins the failure mode the reference calls out:
// retrying a withdrawn model multiplies admissions and starves the tier.
func TestWithdrawnModelIsNotRetried(t *testing.T) {
	var completions atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		completions.Add(1)
		w.WriteHeader(http.StatusGone)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "model_unavailable"})
	}))
	defer server.Close()

	cfg := llm.FromProvider(server.URL+"/api/v1", "token", "withdrawn-model")
	cfg.HTTPClient = server.Client()
	cfg.ExtraBodyFunc = func() map[string]any { return freebuff.NewMetadataBuilder().Body("i1") }
	cfg.RecoverError = freebuff.MakeRecoverError(freebuff.NewSessionManager(freebuff.NewClient()))

	streamer := llm.New(cfg)
	err := streamer.Stream(context.Background(), llm.StreamRequest{
		Messages: []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "hi"}},
	}, func(llm.ProviderResponse) {})
	if err == nil {
		t.Fatal("expected an error for a withdrawn model")
	}
	if completions.Load() != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (a withdrawn model must not be retried)", completions.Load())
	}
	// The user-facing message must explain the model, not leak a raw status line.
	if msg := err.Error(); msg == "" || !strings.Contains(msg, "withdrawn") {
		t.Fatalf("error = %q, want an explanation that the model was withdrawn", msg)
	}
}

// TestRecoveryIsBounded pins that a permanently gated provider cannot spin.
func TestRecoveryIsBounded(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusGone)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "session_expired"})
	}))
	defer server.Close()

	cfg := llm.FromProvider(server.URL+"/api/v1", "token", "m1")
	cfg.HTTPClient = server.Client()
	cfg.ExtraBodyFunc = func() map[string]any { return freebuff.NewMetadataBuilder().Body("i1") }
	cfg.RecoverError = freebuff.MakeRecoverError(freebuff.NewSessionManager(freebuff.NewClient()))

	streamer := llm.New(cfg)
	if err := streamer.Stream(context.Background(), llm.StreamRequest{
		Messages: []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "hi"}},
	}, func(llm.ProviderResponse) {}); err == nil {
		t.Fatal("expected an error when the gate never clears")
	}
	// Two recoveries plus the initial attempt; the plain retry budget must not
	// multiply this further.
	if int(attempts.Load()) > llm.MaxRecoveries+1+cfg.MaxRetries {
		t.Fatalf("attempts = %d, recovery is not bounded", attempts.Load())
	}
}
