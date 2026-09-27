package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/internal/config"
	"ceraa/internal/secret"
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

// doctor reads credentials from the encrypted store, so it has to open it before
// collecting any checks.
//
// Run in a subprocess because the store is a package global: once any earlier test
// has called secret.Init, it stays open for the rest of the process and runDoctor's
// own call becomes unobservable. Asserting in-process therefore passed even with
// the open removed, because the store was already open from a previous test.
//
// The helper points CERAA_HOME at a regular file so the key cannot be created.
// runDoctor must then fail on the store itself, before producing a report.
func TestDoctorFailsWhenTheCredentialStoreCannotBeOpened(t *testing.T) {
	if os.Getenv("CERAA_DOCTER_STORE_HELPER") == "1" {
		// The helper has to carry the verdict out through its exit status. An
		// earlier version called runDoctor and returned, so the helper passed
		// whether or not doctor had failed and the parent learned nothing.
		if err := runDoctor(); err != nil {
			fmt.Println("HELPER-ERR:", err)
			return
		}
		fmt.Println("HELPER-OK: runDoctor returned no error")
		os.Exit(3)
	}
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestDoctorFailsWhenTheCredentialStoreCannotBeOpened")
	cmd.Env = append(os.Environ(),
		"CERAA_DOCTER_STORE_HELPER=1",
		"CERAA_HOME="+blocker,
		"TELEGRAM_BOT_TOKEN=999:TESTTOKENabcdefghijklmnopqrstuvwxyz",
		"TELEGRAM_ALLOWED_USER_ID=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor did not fail on an unopenable credential store:\n%s", out)
	}
	if strings.Contains(string(out), "HELPER-OK") {
		t.Fatalf("doctor succeeded with an unopenable credential store:\n%s", out)
	}
	// The wrap from opening the store, not a per-check failure discovered later.
	// With the open removed, the codex check reported "Codex credential: secret
	// store not initialized" instead, which is a different message.
	if !strings.Contains(string(out), "secret store:") {
		t.Errorf("doctor failed with %q, want it to fail opening the store", out)
	}
}

// A provider check has to be reached and named.
// A subscription provider has no URL that answers a GET, so it is checked by
// resolving its credential. Asserted through the report so that restoring the GET
// probe fails here.
func TestSubscriptionProviderIsCheckedByCredentialNotByURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	seedCodexConfig(t)
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	detail := providerCheckDetail(t)
	if strings.Contains(detail, "HTTP") {
		t.Errorf("the codex check still probes a URL: %q", detail)
	}
	if !strings.Contains(detail, "Codex credential") {
		t.Errorf("provider check said %q, want a credential result", detail)
	}
}

// The provider named in the report must be the one serving, which is the runtime
// selection when the profile was chosen in Telegram. Reading the config field
// alone printed "provider=" with nothing after it, so this asserts on what
// checkConfig reports rather than on the helper it calls.
func TestConfigReportNamesTheEffectiveProvider(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)
	state := config.RuntimeState{Provider: "codex", ProviderProfiles: map[string]config.ProviderProfile{
		"codex": {URL: "https://chatgpt.com/backend-api/codex", Model: "gpt-5.6-luna", Subscription: "codex"},
	}}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dir, "bridge-state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := checkConfig(config.DefaultConfig())
	if !strings.Contains(got.Detail, "provider=codex") {
		t.Errorf("config report said %q, want it to name codex", got.Detail)
	}
	if !strings.Contains(got.Detail, "gpt-5.6-luna") {
		t.Errorf("config report said %q, want the model", got.Detail)
	}
}

// seedCodexConfig writes a Codex provider into the temp data directory.
//
// collectDoctorReport loads the config itself, so a struct passed in would be
// ignored and the check would run against an unconfigured install.
func seedCodexConfig(t *testing.T) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.LLM.URL = "https://chatgpt.com/backend-api/codex"
	cfg.LLM.Model = "gpt-5.6-luna"
	cfg.LLM.APIKey = ""
	cfg.LLM.Subscription = "codex"
	if err := config.Save(cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

func providerCheckDetail(t *testing.T) string {
	t.Helper()
	report := collectDoctorReport(2 * time.Second)
	for _, check := range report.Checks {
		if check.Name == "provider" {
			if check.Detail != "" {
				return check.Detail
			}
		}
	}
	t.Fatalf("no provider check in the report: %+v", report.Checks)
	return ""
}
