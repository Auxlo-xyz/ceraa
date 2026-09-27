package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// A session's stored system message carries the model-identity block, and it was
// written only when the session was empty. Switching provider therefore left the
// record naming the previous model, so the session claimed a provider that was no
// longer serving it. This drives a run, changes the model, drives another, and
// asserts the stored record follows.
func TestStoredSystemMessageFollowsAProviderSwitch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	t.Setenv("CERAA_API_URL", server.URL)
	t.Setenv("CERAA_API_KEY", "test")

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	run := func(model string) {
		t.Setenv("CERAA_MODEL", model)
		runner := New(RunnerConfig{
			SessionID:    "identity-switch",
			SystemPrompt: "You are Ceraa.",
			Model:        llm.ProviderConfig{BaseURL: server.URL, Model: model, MaxRetries: 0},
		}, store, tool.DefaultRegistry())
		if _, err := runner.Run(context.Background(), "hello"); err != nil {
			t.Fatalf("run %s: %v", model, err)
		}
	}

	stored := func() string {
		sess, err := store.Load("identity-switch")
		if err != nil {
			t.Fatal(err)
		}
		if len(sess.Messages) == 0 {
			t.Fatal("session has no messages")
		}
		return sess.Messages[0].Content
	}

	run("model-one")
	first := stored()
	if !strings.Contains(first, "model-one") {
		t.Fatalf("first system message does not name the model: %q", first)
	}

	run("model-two")
	second := stored()
	if strings.Contains(second, "model-one") {
		t.Errorf("the stored system message still names the previous model after a switch: %q", second)
	}
	if !strings.Contains(second, "model-two") {
		t.Errorf("the stored system message was not updated to the new model: %q", second)
	}

	// The stored copy is a record, not an input: the runner strips it from the
	// request, so the live prompt is what carries the identity.
	sess, err := store.Load("identity-switch")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(sess.Messages[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "model-identity") && !strings.Contains(second, "model-identity") {
		t.Log("system message has no identity block; the switch assertion above is the load-bearing one")
	}
}
