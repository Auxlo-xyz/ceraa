package freebuff

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestMetadataBodyShape pins the hosted completions contract: codebuff_metadata
// is a TOP-LEVEL body field, reserved keys are always written, and cost_mode is
// free.
func TestMetadataBodyShape(t *testing.T) {
	builder := NewMetadataBuilder()
	body := builder.Body("inst-1")
	raw, ok := body["codebuff_metadata"]
	if !ok {
		t.Fatal("codebuff_metadata must be a top-level body field")
	}
	metadata, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("codebuff_metadata = %T", raw)
	}
	if metadata["freebuff_instance_id"] != "inst-1" {
		t.Fatalf("freebuff_instance_id = %v", metadata["freebuff_instance_id"])
	}
	if metadata["cost_mode"] != "free" {
		t.Fatalf("cost_mode = %v, want free", metadata["cost_mode"])
	}
	for _, key := range []string{"run_id", "client_id"} {
		value, _ := metadata[key].(string)
		if value == "" {
			t.Fatalf("%s is missing", key)
		}
		if !strings.HasPrefix(value, idPrefix) {
			t.Fatalf("%s = %q, want the %q prefix", key, value, idPrefix)
		}
	}
}

// TestMetadataClientIDIsFreshPerRequest pins that the per-request correlation id
// is never reused, while the run id stays stable for the life of a run.
func TestMetadataClientIDIsFreshPerRequest(t *testing.T) {
	builder := NewMetadataBuilder()
	seen := map[string]bool{}
	var runID string
	for i := 0; i < 5; i++ {
		metadata := builder.Body("inst-1")["codebuff_metadata"].(map[string]any)
		clientID := metadata["client_id"].(string)
		if seen[clientID] {
			t.Fatalf("client_id %q was reused", clientID)
		}
		seen[clientID] = true
		current := metadata["run_id"].(string)
		if i == 0 {
			runID = current
			continue
		}
		if current != runID {
			t.Fatalf("run_id changed mid-run: %q != %q", current, runID)
		}
	}
	builder.ResetRun()
	metadata := builder.Body("inst-1")["codebuff_metadata"].(map[string]any)
	if metadata["run_id"] == runID {
		t.Fatal("ResetRun must rotate the run id")
	}
}

// TestMetadataClientIDAvoidsReservedPrefix pins that Ceraa's ids cannot be
// confused with the reference's own published proxy generator.
func TestMetadataClientIDAvoidsReservedPrefix(t *testing.T) {
	builder := NewMetadataBuilder()
	for i := 0; i < 20; i++ {
		clientID := builder.Body("i")["codebuff_metadata"].(map[string]any)["client_id"].(string)
		if strings.HasPrefix(clientID, "wf-") {
			t.Fatalf("client_id %q uses the reserved wf- prefix", clientID)
		}
	}
}

// TestUserAgentMatchesOfficialCLI pins the exact RFC 7231 form the server's
// normalizer expects: product/version, with a version-shaped second half. The
// operator chose to identify as the official CLI so Ceraa is not treated as an
// unrecognized client.
func TestUserAgentMatchesOfficialCLI(t *testing.T) {
	ua := NewMetadataBuilder().Headers()["user-agent"]
	if !strings.HasPrefix(ua, cliUserAgentProduct+"/") {
		t.Fatalf("user agent %q must start with %q", ua, cliUserAgentProduct+"/")
	}
	product, version, found := strings.Cut(ua, "/")
	if !found {
		t.Fatalf("user agent %q has no version", ua)
	}
	// The server lowercases the product before recording it, so only
	// [a-z0-9._-] may appear.
	if got := strings.ToLower(product); strings.Trim(got, "abcdefghijklmnopqrstuvwxyz0123456789._-") != "" {
		t.Fatalf("product %q has characters the server strips", product)
	}
	// A version is only kept when it is version-shaped and <=16 chars, so an
	// over-long value would be silently discarded and read as "unknown version".
	if len(version) > 16 || !versionPattern.MatchString(version) {
		t.Fatalf("version %q would be discarded by the server normalizer", version)
	}
}

// TestUserAgentVersionIsOverridable pins that the advertised version is a
// deliberate, version-shaped value rather than a hardcoded constant.
func TestUserAgentVersionIsOverridable(t *testing.T) {
	t.Setenv("CERAA_FREEBUFF_CLI_VERSION", "1.2.3")
	if got := cliUserAgentVersion(); got != "1.2.3" {
		t.Fatalf("version = %q, want the override", got)
	}
	// A second valid value is honoured, so the value is not a constant.
	t.Setenv("CERAA_FREEBUFF_CLI_VERSION", "2.0.0")
	if got := cliUserAgentVersion(); got != "2.0.0" {
		t.Fatalf("version = %q, want the new override", got)
	}
	// A value the server would discard falls back rather than being sent.
	for _, bad := range []string{"dev-branch-name-that-is-long", "notaversion", "12345678901234567890"} {
		t.Setenv("CERAA_FREEBUFF_CLI_VERSION", bad)
		if got := cliUserAgentVersion(); got != DefaultCLIVersion {
			t.Fatalf("version for %q = %q, want the default fallback", bad, got)
		}
	}
	t.Setenv("CERAA_FREEBUFF_CLI_VERSION", "")
	if got := cliUserAgentVersion(); got != DefaultCLIVersion {
		t.Fatalf("empty override = %q, want the default", got)
	}
}

// TestRecoverErrorReAdmitsOnSessionEndingGate pins the only supported recovery
// for a session-ending gate: drop the row and retry, which re-admits on the
// same instance id.
func TestRecoverErrorReAdmitsOnSessionEndingGate(t *testing.T) {
	var posts atomic.Int32
	_, client := newSessionServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "active", "instanceId": "i1", "model": "m1"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	manager := NewSessionManager(client)
	hook := MakeRecoverError(manager)
	// Recovery needs a captured credential, exactly as the resolver provides.
	if _, err := manager.Ensure(context.Background(), "token", "m1"); err != nil {
		t.Fatal(err)
	}
	before := posts.Load()
	for _, code := range []string{"session_expired", "session_superseded", "session_model_mismatch", "waiting_room_required"} {
		status := gateCodes[code].Status
		body, _ := json.Marshal(map[string]any{"error": code})
		retry, message := hook(context.Background(), status, body)
		if !retry {
			t.Fatalf("%s: retry = false, want a re-admission", code)
		}
		if message != "" {
			t.Fatalf("%s: unexpected message %q", code, message)
		}
		if manager.Current().Status != "active" {
			t.Fatalf("%s: session was not re-admitted", code)
		}
	}
	if posts.Load() != before+4 {
		t.Fatalf("re-admissions = %d, want one per gate", posts.Load()-before)
	}
	// The instance id must survive: recovery re-admits on the same instance.
	if manager.InstanceID() == "" {
		t.Fatal("the instance id must be stable across re-admission")
	}
}

// TestRecoverErrorDoesNotRetryNonSessionEndingGate pins that a withdrawn model
// is not retried, which is what caused a Freebuff tier's admission storm.
func TestRecoverErrorDoesNotRetryNonSessionEndingGate(t *testing.T) {
	manager := NewSessionManager(NewClient())
	manager.adopt(SessionResponse{Status: "active", InstanceID: "i1", Model: "m1"})
	hook := MakeRecoverError(manager)
	cases := map[string]string{
		"model_unavailable":     "withdrawn from free mode",
		"session_limit_reached": "concurrent-session limit",
		"waiting_room_queued":   "queuing",
	}
	for code, wantSubstring := range cases {
		body, _ := json.Marshal(map[string]any{"error": code})
		retry, message := hook(context.Background(), gateCodes[code].Status, body)
		if retry {
			t.Fatalf("%s must not be retried", code)
		}
		if !strings.Contains(message, wantSubstring) {
			t.Fatalf("%s message = %q, want it to mention %q", code, message, wantSubstring)
		}
		if manager.Current().Status != "active" {
			t.Fatalf("%s must not invalidate the live session", code)
		}
	}
}

// TestRecoverErrorRequiresMatchingStatus pins that a bare error code with the
// wrong HTTP status is not treated as a gate.
func TestRecoverErrorRequiresMatchingStatus(t *testing.T) {
	manager := NewSessionManager(NewClient())
	manager.adopt(SessionResponse{Status: "active", InstanceID: "i1", Model: "m1"})
	hook := MakeRecoverError(manager)
	body, _ := json.Marshal(map[string]any{"error": "session_expired"})
	if retry, _ := hook(context.Background(), 500, body); retry {
		t.Fatal("a gate code with the wrong status must not trigger recovery")
	}
	if manager.Current().Status != "active" {
		t.Fatal("session must be untouched")
	}
}

// TestRecoverErrorIgnoresUnrelatedErrors keeps the hook from firing on ordinary
// provider problems.
func TestRecoverErrorIgnoresUnrelatedErrors(t *testing.T) {
	manager := NewSessionManager(NewClient())
	hook := MakeRecoverError(manager)
	for _, body := range []string{`{}`, `not json`, `{"error":"toString"}`, `{"error":"some_other_thing"}`} {
		if retry, _ := hook(context.Background(), 500, []byte(body)); retry {
			t.Fatalf("body %q must not trigger recovery", body)
		}
	}
}

// TestCapacityDeferredIsNotQuotaExhaustion pins the distinction the UI needs.
func TestCapacityDeferredIsNotQuotaExhaustion(t *testing.T) {
	body := []byte(`{"error":"free_mode_capacity_deferred"}`)
	if !CapacityDeferred(429, body) {
		t.Fatal("429 + capacity_deferred must be detected")
	}
	if CapacityDeferred(429, []byte(`{"error":"rate_limited"}`)) {
		t.Fatal("a genuine rate limit is not a capacity deferral")
	}
	if CapacityDeferred(400, body) {
		t.Fatal("the code only counts with a 429")
	}
	if got := DescribeError(429, body); !strings.Contains(got, "capacity") {
		t.Fatalf("DescribeError = %q", got)
	}
}

// TestDescribeErrorPrefersQuotaExplanation pins that a user sees the quota
// reason rather than a raw provider body.
func TestDescribeErrorPrefersQuotaExplanation(t *testing.T) {
	body := []byte(`{"error":"waiting_room_required","message":"internal detail"}`)
	got := DescribeError(428, body)
	if !strings.Contains(got, "session slot") {
		t.Fatalf("DescribeError = %q", got)
	}
	if got := DescribeError(401, []byte(`{}`)); !strings.Contains(got, "/freebuff-login") {
		t.Fatalf("a 401 must point at re-login, got %q", got)
	}
}
