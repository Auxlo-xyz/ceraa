package cmd

import (
	"bufio"
	"ceraa/harness/llm"
	"ceraa/internal/codexauth"
	"ceraa/internal/envs"
	"ceraa/internal/paths"
	"ceraa/internal/secret"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"ceraa/internal/config"
	"ceraa/internal/hermesauth"
	"ceraa/internal/opencodeauth"
)

type doctorCheck struct {
	Name   string
	Status string
	Detail string
}

type doctorReport struct {
	Checks []doctorCheck
}

func (r doctorReport) print() {
	for _, check := range r.Checks {
		fmt.Printf("[%s] %-18s %s\n", check.Status, check.Name, check.Detail)
	}
}

func (r doctorReport) failed() bool {
	for _, check := range r.Checks {
		if check.Status == "FAIL" {
			return true
		}
	}
	return false
}

func runDoctor() error {
	// Open the encrypted store first. Every credential check reads from it, and
	// without this the checks silently saw an empty store: doctor reported a
	// working Codex install as having no credential, because the file was never
	// decrypted rather than because anything was missing.
	if err := secret.Init(filepath.Join(paths.DataDir(), "secret.key"), filepath.Join(paths.DataDir(), "secrets.json")); err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	report := collectDoctorReport(10 * time.Second)
	report.print()
	if report.failed() {
		return fmt.Errorf("one or more doctor checks failed")
	}
	return nil
}

func collectDoctorReport(timeout time.Duration) doctorReport {
	report := doctorReport{}
	cfg, err := config.Load()
	if err != nil {
		report.Checks = append(report.Checks, doctorCheck{"config", "FAIL", err.Error()})
		return report
	}
	report.Checks = append(report.Checks, doctorCheck{"runtime", "OK", runtime.Version() + " on " + runtime.GOOS + "/" + runtime.GOARCH})
	report.Checks = append(report.Checks, checkConfig(cfg))
	report.Checks = append(report.Checks, checkWorkspace(cfg))
	report.Checks = append(report.Checks, checkProvider(cfg, timeout))
	report.Checks = append(report.Checks, checkTelegram(cfg, timeout))
	report.Checks = append(report.Checks, checkStorage(cfg)...)
	return report
}

// isSubscriptionProvider reports whether a provider carries a subscription
// credential instead of a stored API key.
//
// One definition for both checks below. They previously each hardcoded "hermes",
// so a Codex install was reported as unconfigured by one check while the other
// quietly probed it unauthenticated and called the result reachable.
func isSubscriptionProvider(subscription string) bool { return llm.IsSubscription(subscription) }

func checkConfig(cfg *config.Config) doctorCheck {
	if cfg == nil {
		return doctorCheck{"config", "FAIL", "configuration is nil"}
	}
	provider := cfg.ActiveLLM()
	// Subscription providers carry no API key in the profile; their credential is
	// resolved from the encrypted store at request time, so demanding one here
	// reported a working Codex install as unconfigured.
	subscription := isSubscriptionProvider(provider.Subscription)
	if (provider.URL == "" && !subscription) || provider.Model == "" || (!subscription && provider.APIKey == "" && envs.GetWith("CERAA_API_KEY", "OPENAI_API_KEY") == "") {
		return doctorCheck{"config", "WARN", "provider is not configured; finish onboarding in Telegram"}
	}
	return doctorCheck{"config", "OK", fmt.Sprintf("provider=%s model=%s", cfg.EffectiveProviderID(), provider.Model)}
}

func checkWorkspace(cfg *config.Config) doctorCheck {
	root := cfg.WorkspaceRootPath()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return doctorCheck{"workspace", "FAIL", err.Error()}
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return doctorCheck{"workspace", "FAIL", "workspace root is unavailable"}
	}
	return doctorCheck{"workspace", "OK", root}
}

func checkProvider(cfg *config.Config, timeout time.Duration) doctorCheck {
	provider := cfg.ActiveLLM()
	if provider.Model == "" || (provider.URL == "" && !isSubscriptionProvider(provider.Subscription)) {
		return doctorCheck{"provider", "WARN", "provider is not configured; finish onboarding in Telegram"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// A subscription provider is checked by resolving its credential, not by
	// probing a URL. The Codex endpoint answers POST /responses and nothing else,
	// so a GET on the base URL returns 403 whatever the credential state, which
	// made the probe meaningless while still reporting a result.
	if isSubscriptionProvider(provider.Subscription) {
		return checkSubscriptionCredential(ctx, provider, timeout)
	}

	apiKey := provider.APIKey
	baseURL := provider.URL
	switch provider.Subscription {
	case llm.HermesSubscription:
		credential, err := hermesauth.NewStore().Resolve(ctx, provider.HermesProvider)
		if err != nil {
			return doctorCheck{"provider", "FAIL", "Hermes credential: " + err.Error()}
		}
		apiKey = credential.AccessToken
		baseURL = credential.BaseURL
	}
	if !validProviderURL(baseURL) {
		return doctorCheck{"provider", "FAIL", "provider URL must use http:// or https://"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/"), nil)
	if err != nil {
		return doctorCheck{"provider", "FAIL", err.Error()}
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return doctorCheck{"provider", "FAIL", "cannot reach provider: " + err.Error()}
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 500:
		// A 4xx still means the endpoint answered, which is all this proves. Saying
		// "reachable" and nothing more, because that is what was checked.
		return doctorCheck{"provider", "OK", fmt.Sprintf("endpoint answered (HTTP %d)", resp.StatusCode)}
	default:
		return doctorCheck{"provider", "FAIL", fmt.Sprintf("provider returned HTTP %d", resp.StatusCode)}
	}
}

// checkSubscriptionCredential resolves a subscription provider's stored token.
//
// Resolving rather than merely reading also proves the credential is usable: the
// Codex manager refreshes an expired access token against the stored refresh token
// while doing this, so a successful resolve is evidence the login still works.
func checkSubscriptionCredential(ctx context.Context, provider config.LLMProvider, timeout time.Duration) doctorCheck {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch provider.Subscription {
	case llm.CodexSubscription:
		token, err := codexauth.AccessToken(deadline)
		if err != nil {
			return doctorCheck{"provider", "FAIL", "Codex credential: " + err.Error()}
		}
		if strings.TrimSpace(token) == "" {
			return doctorCheck{"provider", "FAIL", "Codex credential is empty; sign in again"}
		}
		return doctorCheck{"provider", "OK", "Codex credential resolves" + codexExpirySuffix()}
	case llm.HermesSubscription:
		if _, err := hermesauth.NewStore().Resolve(deadline, provider.HermesProvider); err != nil {
			return doctorCheck{"provider", "FAIL", "Hermes credential: " + err.Error()}
		}
		return doctorCheck{"provider", "OK", "Hermes credential resolves for " + provider.HermesProvider}
	case llm.OpenCodeSubscription:
		// A static bearer token, so unlike the OAuth providers there is nothing
		// to refresh and a resolve is a presence check rather than proof the
		// grant still works. Proving the key works would mean a completions
		// call, which bills the account, and doctor is not the place for that.
		credential, err := opencodeauth.CredentialFor(deadline, provider.APIKey)
		if err != nil {
			return doctorCheck{"provider", "FAIL", "OpenCode credential: " + err.Error()}
		}
		detail := "OpenCode Zen credential resolves"
		if credential.ProjectID != "" {
			detail += " (project " + credential.ProjectID + ")"
		}
		return doctorCheck{"provider", "OK", detail}
	}
	return doctorCheck{"provider", "WARN", "unknown subscription provider: " + provider.Subscription}
}

// codexExpirySuffix names the stored expiry, so the check says how much runway is
// left rather than only that a token exists.
func codexExpirySuffix() string {
	raw, err := secret.Get("codex_expires_at")
	if err != nil || raw == "" {
		return ""
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return ""
	}
	return ", valid until " + time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}

func checkTelegram(cfg *config.Config, timeout time.Duration) doctorCheck {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		token = cfg.Telegram.BotToken
	}
	if token == "" {
		return doctorCheck{"telegram", "FAIL", "TELEGRAM_BOT_TOKEN is missing"}
	}
	allow := os.Getenv("TELEGRAM_ALLOWED_USER_ID")
	if allow != "" {
		if _, err := strconv.ParseInt(allow, 10, 64); err != nil {
			return doctorCheck{"telegram", "FAIL", "TELEGRAM_ALLOWED_USER_ID is not a valid integer"}
		}
	} else if cfg.Telegram.AllowedUserID == 0 {
		return doctorCheck{"telegram", "WARN", "Telegram is configured without an allowlist"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.telegram.org/bot"+token+"/getMe", nil)
	if err != nil {
		return doctorCheck{"telegram", "FAIL", err.Error()}
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return doctorCheck{"telegram", "FAIL", "cannot reach Telegram: " + err.Error()}
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return doctorCheck{"telegram", "FAIL", fmt.Sprintf("Telegram returned HTTP %d", resp.StatusCode)}
	}
	return doctorCheck{"telegram", "OK", "Telegram token accepted"}
}

func checkStorage(cfg *config.Config) []doctorCheck {
	checks := make([]doctorCheck, 0, 5)
	checks = append(checks, checkDirectory("sessions", cfg.SessionFile))
	checks = append(checks, checkDirectory("memory", cfg.MemoryFile))
	checks = append(checks, checkDirectory("skills", cfg.SkillFile))
	checks = append(checks, checkParent("evaluation", cfg.EvaluationFile))
	checks = append(checks, checkParent("scheduler", cfg.SchedulerFile))
	return checks
}

func checkDirectory(name, path string) doctorCheck {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return doctorCheck{name, "FAIL", err.Error()}
	}
	file, err := os.CreateTemp(path, ".doctor-*")
	if err != nil {
		return doctorCheck{name, "FAIL", err.Error()}
	}
	tempName := file.Name()
	_ = file.Close()
	_ = os.Remove(tempName)
	return doctorCheck{name, "OK", path}
}

func checkParent(name, path string) doctorCheck {
	if path == "" {
		return doctorCheck{name, "FAIL", "path is empty"}
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return doctorCheck{name, "FAIL", err.Error()}
	}
	file, err := os.CreateTemp(parent, ".doctor-*")
	if err != nil {
		return doctorCheck{name, "FAIL", err.Error()}
	}
	tempName := file.Name()
	_ = file.Close()
	_ = os.Remove(tempName)
	return doctorCheck{name, "OK", path}
}

func validProviderURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func runSetup(reader io.Reader, writer io.Writer) error {
	// The persisted config, not the resolved one: saving the resolved config would
	// write this shell's CERAA_* overrides into the file and make them permanent.
	cfg, err := config.LoadPersisted()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if writer == nil {
		writer = os.Stdout
	}
	if reader == nil {
		reader = os.Stdin
	}
	input := bufio.NewReader(reader)
	fmt.Fprintln(writer, "Ceraa setup")
	fmt.Fprintln(writer, "Enter only the Telegram bot token. Provider, model, API key, and allowlist are completed in Telegram onboarding.")
	telegramToken := askSetup(input, writer, "Telegram bot token", firstNonEmpty(os.Getenv("TELEGRAM_BOT_TOKEN"), cfg.Telegram.BotToken), func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("Telegram bot token is required")
		}
		return nil
	})
	envTelegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if envTelegramToken != "" && telegramToken == envTelegramToken {
		telegramToken = ""
	}
	cfg.Telegram.BotToken = telegramToken
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintln(writer, "Telegram token saved. Run `ceraa start`, then finish onboarding in Telegram.")
	return nil
}

func askSetup(input *bufio.Reader, writer io.Writer, label, current string, validate func(string) error) string {
	for {
		if current == "" {
			fmt.Fprintf(writer, "%s: ", label)
		} else {
			fmt.Fprintf(writer, "%s [%s]: ", label, current)
		}
		line, err := input.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return current
		}
		value := strings.TrimSpace(line)
		if value == "" {
			value = current
		}
		if validate != nil {
			if err := validate(value); err != nil {
				fmt.Fprintf(writer, "%s\n", err)
				continue
			}
		}
		return value
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
