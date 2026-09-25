package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/internal/config"
)

func TestDefaultConfigHasNoProviderOrModel(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.LLM.URL != "" || cfg.LLM.Model != "" || cfg.ActiveProvider != "" {
		t.Fatalf("unexpected provider defaults: %+v", cfg)
	}
}

func TestValidProviderURL(t *testing.T) {
	for _, value := range []string{"https://api.example.test/v1", "http://localhost:1234/v1"} {
		if !validProviderURL(value) {
			t.Errorf("expected valid provider URL %q", value)
		}
	}
	for _, value := range []string{"", "api.example.test", "ftp://api.example.test"} {
		if validProviderURL(value) {
			t.Errorf("expected invalid provider URL %q", value)
		}
	}
}

func TestCheckDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions")
	check := checkDirectory("sessions", path)
	if check.Status != "OK" {
		t.Fatalf("unexpected storage check: %+v", check)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestRunSetupPersistsConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("CERAA_API_KEY", "")
	t.Setenv("SOORAA_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	input := strings.NewReader("123456:telegram\n")
	if err := runSetup(input, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	// Writes always go to $HOME/.config/ceraa; XDG is a read candidate only.
	configPath := filepath.Join(home, ".config", "ceraa", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "123456:telegram") {
		t.Fatalf("saved config is missing the Telegram token: %s", data)
	}
	if strings.Contains(string(data), "provider/model") || strings.Contains(string(data), "api.example.test") {
		t.Fatalf("setup unexpectedly saved provider defaults: %s", data)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %o", info.Mode().Perm())
	}
}
