package cmd

import (
	"bufio"
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

	"sooraa/internal/config"
	"sooraa/internal/freebuff"
	"sooraa/internal/hermesauth"
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
	report.Checks = append(report.Checks, doctorCheck{"freebuff", "INFO", freebuffSummary()})
	report.Checks = append(report.Checks, checkTelegram(cfg, timeout))
	report.Checks = append(report.Checks, checkStorage(cfg)...)
	return report
}

func checkConfig(cfg *config.Config) doctorCheck {
	if cfg == nil {
		return doctorCheck{"config", "FAIL", "configuration is nil"}
	}
	provider := cfg.ActiveLLM()
	// Subscription providers carry no API key in the profile; their credential
	// is resolved from the encrypted native store at request time.
	subscription := provider.Subscription == "hermes" || provider.Subscription == "freebuff"
	if (provider.URL == "" && !subscription) || provider.Model == "" || (!subscription && provider.APIKey == "" && os.Getenv("SOORAA_API_KEY") == "" && os.Getenv("OPENAI_API_KEY") == "") {
		return doctorCheck{"config", "WARN", "provider is not configured; finish onboarding in Telegram"}
	}
	return doctorCheck{"config", "OK", fmt.Sprintf("provider=%s model=%s", cfg.ActiveProvider, provider.Model)}
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
	if provider.Model == "" || (provider.URL == "" && provider.Subscription != "hermes" && provider.Subscription != "freebuff") {
		return doctorCheck{"provider", "WARN", "provider is not configured; finish onboarding in Telegram"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	apiKey := provider.APIKey
	baseURL := provider.URL
	switch provider.Subscription {
	case "hermes":
		credential, err := hermesauth.NewStore().Resolve(ctx, provider.HermesProvider)
		if err != nil {
			return doctorCheck{"provider", "FAIL", "Hermes credential: " + err.Error()}
		}
		apiKey = credential.AccessToken
		baseURL = credential.BaseURL
	case "freebuff":
		// Freebuff's inference root is a hosted path; probe /api/v1/me instead
		// so a valid token is distinguishable from an unreachable host.
		credential, err := freebuff.NewStore().Resolve(ctx)
		if err != nil {
			return doctorCheck{"provider", "FAIL", "Freebuff credential: " + err.Error() + " (run: sooraa freebuff login)"}
		}
		apiKey = credential.AccessToken
		baseURL = freebuff.DefaultAPIBaseURL
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
		return doctorCheck{"provider", "OK", fmt.Sprintf("provider reachable (HTTP %d)", resp.StatusCode)}
	default:
		return doctorCheck{"provider", "FAIL", fmt.Sprintf("provider returned HTTP %d", resp.StatusCode)}
	}
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
	cfg, err := config.Load()
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
	fmt.Fprintln(writer, "Sooraa setup")
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
	fmt.Fprintln(writer, "Telegram token saved. Run `sooraa start`, then finish onboarding in Telegram.")
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
