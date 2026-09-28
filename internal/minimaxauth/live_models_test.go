package minimaxauth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/internal/paths"
	"ceraa/internal/secret"
)

// Read the live service only when asked: it touches a third party and spends
// plan credits.
func liveInferenceDisabled() bool { return os.Getenv("CERAA_LIVE_INFERENCE") != "1" }

// openLiveSecrets opens the real encrypted store. Called only from the live path,
// so an ordinary test run never reads the user's credentials.
func openLiveSecrets(t *testing.T) {
	t.Helper()
	dir := paths.DataDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("open secrets: %v", err)
	}
}

// The Messages API answers 200 to any model id and quietly serves MiniMax-M3, so
// a request that returns 200 proves nothing about which model ran. The response
// echoes the model it actually used, and that field is the only reliable check.
//
// The account's own /v1/models list is not the authority either: it omits
// MiniMax-M3.1-Flash-Preview, which this confirms is served. Both directions of
// that failure are expensive — a real model looks unavailable, and an absent one
// looks available — so the ids we ship are checked against the echo.
func TestLiveEveryServedModelIDIsHonoured(t *testing.T) {
	if liveInferenceDisabled() {
		t.Skip("live inference disabled")
	}
	openLiveSecrets(t)
	cred, err := Load(RegionGlobal)
	if err != nil {
		t.Skipf("no stored credential: %v", err)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	endpoint := InferenceBaseURL(RegionGlobal) + "/v1/messages"

	for _, model := range llm.MiniMaxModels {
		payload, _ := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 8,
			"messages":   []map[string]any{{"role": "user", "content": "Reply with the single word: ok"}},
		})
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", cred.AccessToken)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("%s: %v", model, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4000))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: HTTP %d: %s", model, resp.StatusCode, body)
			continue
		}
		var decoded struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("%s: response is not JSON: %s", model, body)
			continue
		}
		if decoded.Model != model {
			t.Errorf("%s: the endpoint answered 200 but ran %q; this id is a phantom that "+
				"silently gets MiniMax-M3, so shipping it would be a model we never used",
				model, decoded.Model)
		}
	}
}
