package cmd

import (
	"slices"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/config"
)

// A mistyped subcommand used to be sent to the model as a prompt, so `ceraa
// stopp` cost a real inference call and returned a nonsense answer.
func TestNearMissCatchesTypos(t *testing.T) {
	commands := []string{"version", "start", "stop", "setup", "doctor", "config", "config-init"}
	// confi is one insertion from config-init, so it is caught at the narrow
	// tolerance. confiu is two edits from every subcommand and needs the wider one,
	// which is why the threshold scales with command length.
	for _, typo := range []string{"stopp", "strt", "startt", "docto", "confi", "confiu", "versoin", "stat"} {
		got, ok := nearMiss(typo, commands)
		if !ok {
			t.Errorf("%q was not caught as a typo", typo)
			continue
		}
		if !slices.Contains(commands, got) {
			t.Errorf("%q suggested %q, which is not a real subcommand", typo, got)
		}
	}
}

// The guard must not swallow real prompts. Short words sit close to short
// subcommands in edit space, so this is where a naive threshold breaks: `ok` is
// two edits from `stop`.
func TestNearMissLeavesRealPromptsAlone(t *testing.T) {
	commands := []string{"version", "start", "stop", "setup", "doctor", "config", "config-init"}
	for _, prompt := range []string{"ok", "hi", "go", "run", "fix", "it", "no", "yes", "tests", "help"} {
		if got, ok := nearMiss(prompt, commands); ok {
			t.Errorf("%q is a legitimate prompt but was refused as a typo of %q", prompt, got)
		}
	}
}

// Anything with whitespace is unambiguously a prompt.
func TestNearMissIgnoresMultiWordPrompts(t *testing.T) {
	commands := []string{"stop", "start"}
	for _, prompt := range []string{"stop the server", "start now", "config please"} {
		if got, ok := nearMiss(prompt, commands); ok {
			t.Errorf("%q was refused as a typo of %q", prompt, got)
		}
	}
}

// The wider tolerance for longer commands has to be load-bearing: at a flat
// threshold of one, a two-edit typo is no longer caught and silently becomes an
// inference call again.
func TestTwoEditTyposNeedTheWiderTolerance(t *testing.T) {
	// Registered here because the recorded list is populated by
	// registerSubcommands, which Execute calls but a unit test does not.
	registerSubcommands()
	commands := subcommandNames()
	if len(commands) == 0 {
		t.Fatal("no subcommands recorded; the guard would silently pass everything")
	}
	if _, ok := nearMiss("confiu", commands); !ok {
		t.Error("a two-edit typo of a long subcommand was not caught")
	}
	if toleranceFor("config") <= toleranceFor("stop") {
		t.Error("tolerance does not widen with command length")
	}
}

// A transposition is one edit, not two, or `strt` for `start` falls outside the
// threshold and the commonest typo of all goes through to the model.
func TestTranspositionCostsOneEdit(t *testing.T) {
	if d := damerauLevenshtein("strt", "start"); d != 1 {
		t.Errorf("distance(strt, start) = %d, want 1", d)
	}
	if d := damerauLevenshtein("stopp", "stop"); d != 1 {
		t.Errorf("distance(stopp, stop) = %d, want 1", d)
	}
	if d := damerauLevenshtein("abc", "abc"); d != 0 {
		t.Errorf("distance of identical strings = %d, want 0", d)
	}
}

// The recorded names must match what is actually registered, or the guard drifts
// from the CLI.
func TestKnownSubcommandsAreRecorded(t *testing.T) {
	registerSubcommands()
	recorded := subcommandNames()
	for _, want := range []string{"version", "start", "stop", "setup", "doctor", "config", "config-init"} {
		if !slices.Contains(recorded, want) {
			t.Errorf("%q is not recorded for the near-miss guard: %v", want, recorded)
		}
	}
	for _, unwanted := range []string{"help", "completion"} {
		if slices.Contains(recorded, unwanted) {
			t.Errorf("%q should be excluded from the near-miss guard: %v", unwanted, recorded)
		}
	}
}

// The telegram alias was byte-identical to start, so it was a second name for one
// daemon rather than a second command.
func TestTelegramAliasIsGone(t *testing.T) {
	registerSubcommands()
	if slices.Contains(subcommandNames(), "telegram") {
		t.Error("the duplicate telegram alias is still registered")
	}
	for _, command := range root.Commands() {
		if command.Name() == "telegram" {
			t.Error("a telegram subcommand is still attached to root")
		}
	}
}

// setup used to save the resolved config, which writes this shell's CERAA_*
// overrides into the file and makes them permanent.
func TestSetupDoesNotPersistEnvironmentOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	t.Setenv("CERAA_API_URL", "https://env.example/v1")
	t.Setenv("CERAA_MODEL", "env/model")
	t.Setenv("CERAA_MAX_OUTPUT_TOKENS", "4096")

	// A file with a value worth keeping, so the test can tell persistence apart
	// from a wholesale reset.
	existing := config.DefaultConfig()
	existing.LLM.Model = "from-file"
	existing.LLM.URL = "https://file.example/v1"
	if err := config.Save(existing); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := runSetup(strings.NewReader("999:TOKENabcdefghijklmnopqrstuv\n"), nil); err != nil {
		t.Fatalf("setup: %v", err)
	}

	saved, err := config.LoadPersisted()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if saved.LLM.URL == "https://env.example/v1" || saved.LLM.Model == "env/model" {
		t.Errorf("environment overrides were written to config.json: url=%q model=%q", saved.LLM.URL, saved.LLM.Model)
	}
	if saved.LLM.MaxOutputTokens == 4096 {
		t.Error("CERAA_MAX_OUTPUT_TOKENS was written to config.json")
	}
	if saved.LLM.URL != "https://file.example/v1" || saved.LLM.Model != "from-file" {
		t.Errorf("setup discarded the stored provider: url=%q model=%q", saved.LLM.URL, saved.LLM.Model)
	}
	if saved.Telegram.BotToken == "" {
		t.Error("setup did not save the token it was given")
	}
}

// A subscription provider keeps its credential in the encrypted store, so a check
// demanding a profile API key reported a working Codex install as unconfigured.
// That warning then sent an authenticated user back to the provider picker.
func TestSubscriptionProvidersCountAsConfigured(t *testing.T) {
	if !llm.IsSubscription(llm.CodexSubscription) {
		t.Error("codex is not recognised as a subscription provider")
	}
	if !llm.IsSubscription(llm.HermesSubscription) {
		t.Error("hermes is not recognised as a subscription provider")
	}
	if llm.IsSubscription("openrouter") {
		t.Error("a plain API-key provider was treated as a subscription")
	}

	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	cfg := config.DefaultConfig()
	cfg.LLM.URL = llm.CodexBaseURL
	cfg.LLM.Model = "gpt-5.6-luna"
	// Deliberately no API key: that is the shape of a subscription profile.
	cfg.LLM.APIKey = ""
	cfg.LLM.Subscription = llm.CodexSubscription

	if got := checkConfig(cfg); got.Status == "WARN" {
		t.Errorf("a configured subscription provider warned: %q", got.Detail)
	}
}

func TestPlainProviderStillNeedsAnAPIKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	cfg := config.DefaultConfig()
	cfg.LLM.URL = "https://api.example/v1"
	cfg.LLM.Model = "some/model"
	cfg.LLM.APIKey = ""
	cfg.LLM.Subscription = ""
	t.Setenv("CERAA_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	if got := checkConfig(cfg); got.Status != "WARN" {
		t.Errorf("a provider with no key did not warn: %+v", got)
	}
}
