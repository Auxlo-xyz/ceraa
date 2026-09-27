package hermesauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/internal/secret"
)

func liveDisabled() bool { return os.Getenv("CERAA_LIVE_OAUTH") != "1" }

// catalogEntry is one model record. The endpoint's shape is not published, so
// both the `id` and the bare `models` array are accepted rather than assumed.
type catalogEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// TestLiveModelCatalog reads the model list the Nous inference endpoint serves to
// this account, so the compiled list is checked against the endpoint instead of
// trusted. Skipped by default because it touches a third party.
func TestLiveModelCatalog(t *testing.T) {
	if liveDisabled() {
		t.Skip("live OAuth disabled")
	}
	dir := os.Getenv("CERAA_HOME")
	if dir == "" {
		t.Fatal("CERAA_HOME must be set; refusing to run")
	}
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	credential, err := NewStore().Resolve(ctx, "nous")
	if err != nil {
		t.Fatalf("resolve credential: %v", err)
	}
	if credential.BaseURL == "" {
		t.Fatal("the resolved credential has no inference base URL")
	}

	endpoint := strings.TrimSuffix(credential.BaseURL, "/") + "/models"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set(HeaderUserAgent, ClientUserAgent)
	if credential.AccessToken != "" {
		request.Header.Set("Authorization", credential.TokenType+" "+credential.AccessToken)
	}
	response, err := (&http.Client{Timeout: 45 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("fetch catalog: %v", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("catalog returned HTTP %d: %.200s", response.StatusCode, raw)
	}

	var payload struct {
		Data   []catalogEntry `json:"data"`
		Models []catalogEntry `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode catalog: %v (body: %.200s)", err, raw)
	}

	entries := payload.Data
	if len(entries) == 0 {
		entries = payload.Models
	}
	t.Logf("catalog: %d entries from %s", len(entries), endpoint)
	if len(entries) == 0 {
		t.Fatalf("the account was served an empty catalog: %.200s", raw)
	}
	for _, entry := range entries {
		t.Logf("  %-42s owned_by=%s", entry.ID, entry.OwnedBy)
	}

	served := map[string]bool{}
	for _, entry := range entries {
		served[entry.ID] = true
	}
	for _, known := range llm.HermesModels {
		if !served[known] {
			t.Errorf("compiled model %q is not served by the endpoint", known)
		}
	}
	if !served[llm.DefaultHermesModel] {
		t.Errorf("default model %q is not served by the endpoint", llm.DefaultHermesModel)
	}
}
