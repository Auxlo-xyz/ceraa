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
	report.Checks = append(report.Checks, checkWorkspace())
	report.Checks = append(report.Checks, checkProvider(cfg, timeout))
	report.Checks = append(report.Checks, checkTelegram(cfg, timeout))
	report.Checks = append(report.Checks, checkStorage(cfg)...)
	return report
}

func checkConfig(cfg *config.Config) doctorCheck {
	if cfg == nil {
		return doctorCheck{"config", "FAIL", "configuration is nil"}
	}
	if cfg.LLM.URL == "" {
		return doctorCheck{"config", "FAIL", "provider URL is empty"}
	}
	if cfg.LLM.Model == "" {
		return doctorCheck{"config", "FAIL", "model is empty"}
	}
	if cfg.LLM.APIKey == "" && os.Getenv("SOORAA_API_KEY") == "" && os.Getenv("OPENAI_API_KEY") == "" {
		return doctorCheck{"config", "FAIL", "provider API key is missing"}
	}
	return doctorCheck{"config", "OK", fmt.Sprintf("provider=%s model=%s", cfg.ActiveProvider, cfg.LLM.Model)}
}

func checkWorkspace() doctorCheck {
	wd, err := os.Getwd()
	if err != nil {
		return doctorCheck{"workspace", "FAIL", err.Error()}
	}
	info, err := os.Stat(wd)
	if err != nil || !info.IsDir() {
		return doctorCheck{"workspace", "FAIL", "current directory is unavailable"}
	}
	return doctorCheck{"workspace", "OK", wd}
}

func checkProvider(cfg *config.Config, timeout time.Duration) doctorCheck {
	if !validProviderURL(cfg.LLM.URL) {
		return doctorCheck{"provider", "FAIL", "provider URL must use http:// or https://"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.LLM.URL, "/"), nil)
	if err != nil {
		return doctorCheck{"provider", "FAIL", err.Error()}
	}
	if cfg.LLM.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.LLM.APIKey)
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
	fmt.Fprintln(writer, "Press Enter to keep the current value. Credentials already supplied through environment variables will not be written to disk.")

	apiURL := askSetup(input, writer, "Provider API URL", cfg.LLM.URL, func(value string) error {
		if !validProviderURL(value) {
			return fmt.Errorf("URL must start with http:// or https://")
		}
		return nil
	})
	model := askSetup(input, writer, "Model", cfg.LLM.Model, func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("model cannot be empty")
		}
		return nil
	})
	apiKey := askSetup(input, writer, "API key (environment value is kept)", firstNonEmpty(os.Getenv("SOORAA_API_KEY"), os.Getenv("OPENAI_API_KEY"), cfg.LLM.APIKey), nil)
	telegramToken := askSetup(input, writer, "Telegram bot token (environment value is kept)", firstNonEmpty(os.Getenv("TELEGRAM_BOT_TOKEN"), cfg.Telegram.BotToken), nil)
	allowText := askSetup(input, writer, "Telegram allowed user ID", firstNonEmpty(os.Getenv("TELEGRAM_ALLOWED_USER_ID"), strconv.FormatInt(cfg.Telegram.AllowedUserID, 10)), func(value string) error {
		if value == "" {
			return fmt.Errorf("user ID is required")
		}
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return fmt.Errorf("user ID must be numeric")
		}
		return nil
	})
	allowID, err := strconv.ParseInt(allowText, 10, 64)
	if err != nil {
		return err
	}
	envAPIKey := firstNonEmpty(os.Getenv("SOORAA_API_KEY"), os.Getenv("OPENAI_API_KEY"))
	if envAPIKey != "" && apiKey == envAPIKey {
		apiKey = ""
	}
	envTelegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if envTelegramToken != "" && telegramToken == envTelegramToken {
		telegramToken = ""
	}
	cfg.LLM.URL = apiURL
	cfg.LLM.Model = model
	cfg.LLM.APIKey = apiKey
	cfg.Telegram.BotToken = telegramToken
	cfg.Telegram.AllowedUserID = allowID
	cfg.ActiveProvider = "default"
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(writer, "Saved configuration. Run `sooraa doctor` to verify connectivity.\n")
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
