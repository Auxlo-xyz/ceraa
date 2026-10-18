package telegram

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/config"
	"ceraa/internal/opencodeauth"
	"ceraa/internal/secret"
)

// telegramCapture stands in for the Telegram API so the send surface can be
// seen from a test the way callback_test does. Bodies come back in call order:
// every test here makes its calls synchronously, so the handler has finished
// for one call before the next begins.
type telegramCapture struct {
	bodies []string
}

func newTelegramCapture(t *testing.T) *telegramCapture {
	t.Helper()
	cap := &telegramCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw, err := io.ReadAll(r.Body); err == nil && strings.Contains(string(raw), `"text"`) {
			var body struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(raw, &body)
			cap.bodies = append(cap.bodies, body.Text)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	t.Cleanup(server.Close)
	oldBase := telegramAPIBase
	telegramAPIBase = server.URL + "/bot"
	t.Cleanup(func() { telegramAPIBase = oldBase })
	return cap
}

// The not-signed-in branch of the entry point must land on the free tier, not
// on the key prompt. The keyless grant is what the CLI itself serves on with
// no account, and the profile it writes has to be the one the request resolves
// through: public route, whitelist model.
func TestOpenCodeEntryActivatesTheFreeTierWithoutAKey(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	newTelegramCapture(t)
	openCodeEntryPoint("test-token", 42)

	profile, ok := currentProfile(llm.OpenCodeSubscription)
	if !ok {
		t.Fatal("no OpenCode profile was written by the free entry")
	}
	if profile.URL != llm.OpenCodePublicBaseURL {
		t.Errorf("URL = %q, want %q; a keyed-root profile is a guaranteed refusal without a key",
			profile.URL, llm.OpenCodePublicBaseURL)
	}
	if profile.Model != llm.DefaultOpenCodePublicModel {
		t.Errorf("Model = %q, want the default selected from the whitelist", profile.Model)
	}
	if profile.Subscription != llm.OpenCodeSubscription {
		t.Errorf("Subscription = %q, want %q; the resolver would lose the header contract",
			profile.Subscription, llm.OpenCodeSubscription)
	}
}

// The activator keeps a stored model that the free whitelist serves: activation
// is not a model picker, and a user's existing choice is not a bug.
func TestOpenCodeFreeActivationKeepsAWhitelistModel(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	if err := defaultState.setProviderProfile(llm.OpenCodeSubscription, config.ProviderProfile{
		URL: llm.OpenCodePublicBaseURL, Model: "space-bunny-free",
		Subscription: llm.OpenCodeSubscription,
	}); err != nil {
		t.Fatal(err)
	}

	newTelegramCapture(t)
	activateOpenCodePublicProvider("test-token", 42, "")

	profile, _ := currentProfile(llm.OpenCodeSubscription)
	if profile.Model != "space-bunny-free" {
		t.Errorf("Model = %q, want space-bunny-free kept from the stored profile", profile.Model)
	}
}

// A model outside the free whitelist is replaced by the free default rather
// than sent: the keyed free list carries ids the public route does not, and the
// resolution path would answer 401 on the first real usage. big-pickle is on
// the keyed list and not on the measured public whitelist.
func TestOpenCodeFreeActivationReplacesAnOutsideModel(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	if err := defaultState.setProviderProfile(llm.OpenCodeSubscription, config.ProviderProfile{
		URL: llm.OpenCodeBaseURL, Model: "big-pickle",
		Subscription: llm.OpenCodeSubscription,
	}); err != nil {
		t.Fatal(err)
	}

	newTelegramCapture(t)
	activateOpenCodePublicProvider("test-token", 42, "")

	profile, _ := currentProfile(llm.OpenCodeSubscription)
	if profile.Model != llm.DefaultOpenCodePublicModel {
		t.Errorf("Model = %q, want the free default; big-pickle is not measured on the public route", profile.Model)
	}
	if profile.URL != llm.OpenCodePublicBaseURL {
		t.Errorf("URL = %q, want the free tier's own route after re-activation", profile.URL)
	}
}

// A keyed grant keeps the whole catalog: with a stored key the whitelist check
// must not fire, or a paying user would be told free-tier rules.
func TestOpenCodeModelPickKeepsTheCatalogWithAKey(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	openCodeSecretStore(t)
	if err := opencodeauth.Save(opencodeauth.Credential{APIKey: "zk-catalog"}); err != nil {
		t.Fatal(err)
	}

	newTelegramCapture(t)
	selectSubscriptionModel("test-token", 42, "opencode", llm.DefaultOpenCodeModel)

	profile, ok := currentProfile(llm.OpenCodeSubscription)
	if !ok {
		t.Fatal("the keyed pick wrote no profile")
	}
	if profile.Model != llm.DefaultOpenCodeModel {
		t.Errorf("Model = %q, want %q accepted under the keyed grant", profile.Model, llm.DefaultOpenCodeModel)
	}
}

// And the same pick without a key is refused before it is written: the
// resolution path would put a keyed id on the public route and answer 401 on
// the first task, the exact failure a user cannot debug from error text.
// The refusal test needs a keyed-only id, so it uses big-pickle; a model on
// both whitelists, like space-bunny-free, is legitimately accepted free.
func TestOpenCodeModelPickRestrictsTheFreeGrant(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	// An owned store rather than whatever the suite left loaded: a previous
	// test's key is exactly the grant whose reuse this test catches, so it
	// must begin with none.
	openCodeSecretStore(t)

	selectSubscriptionModel("test-token", 42, "opencode", "big-pickle")

	if _, ok := currentProfile(llm.OpenCodeSubscription); ok {
		t.Error("a keyed-only id was written without a key to serve it")
	}
}

// openCodeSecretStore initializes the encrypted store under a temp directory,
// replacing any store a previous test left loaded, so a test owns its grant.
func openCodeSecretStore(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
}
