package config

import (
	"os"
	"os/exec"
	"path/filepath"
)

// ResolveProvider resolves a provider's settings with env var overrides.
// Env format: SORAA_<KEY>_API_KEY, SORAA_<KEY>_CLI
// Supported keys: "opencode", "hermes", "codex"
func ResolveProvider(key string) (SubProvider, bool) {
	switch key {
	case "opencode", "hermes", "codex":
	default:
		return SubProvider{}, false
	}

	cfg, _ := Load()
	p, ok := cfg.Subscriptions.Providers[key]
	if !ok {
		p = SubProvider{Kind: key}
	}

	// Env prefix: SORAA_OPENCODE_, SORAA_HERMES_, SORAA_CODEX_
	envPrefix := "SORAA_" + upper(key) + "_"

	if k := os.Getenv(envPrefix + "API_KEY"); k != "" {
		p.APIKey = k
	}

	if c := os.Getenv(envPrefix + "CLI"); c != "" {
		p.CLIPath = c
	}

	// Resolve default CLI path if empty
	if p.CLIPath == "" {
		if path := findCLI(key); path != "" {
			p.CLIPath = path
		}
	}

	if p.AuthDir == "" {
		p.AuthDir = defaultAuthDir(key)
	}

	return p, true
}

func upper(s string) string {
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		result[i] = c
	}
	return string(result)
}

func findCLI(kind string) string {
	home, _ := os.UserHomeDir()
	candidates := []string{}

	switch kind {
	case "opencode":
		candidates = []string{
			"opencode",
			filepath.Join(home, ".bun", "bin", "opencode"),
			filepath.Join(home, ".local", "bin", "opencode"),
		}
	case "hermes":
		candidates = []string{
			"hermes",
			filepath.Join(home, ".bun", "bin", "hermes"),
		}
	case "codex":
		candidates = []string{
			"codex",
			filepath.Join(home, ".local", "bin", "codex"),
		}
	}

	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
		if fileExists(c) {
			return c
		}
	}
	return ""
}

func defaultAuthDir(kind string) string {
	home, _ := os.UserHomeDir()
	switch kind {
	case "opencode":
		return filepath.Join(home, ".config", "opencode")
	case "hermes":
		return filepath.Join(home, ".hermes")
	case "codex":
		return filepath.Join(home, ".codex")
	default:
		return ""
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
