package config

import (
	"os"
	"os/exec"
	"path/filepath"
)

// ResolveProvider resolves a provider's settings with env var overrides.
// Env format: SOORAA_<KEY>_API_KEY, SOORAA_<KEY>_CLI
// Supported keys: "opencode", "hermes", "codex", "poolside", "freebuff"
//
// Freebuff has no CLI binary, so no CLIPath is resolved for it. Its hosted API
// base URL is intentionally not env-overridable: a process able to set one
// variable could otherwise redirect a bearer token to a host of its choosing.
func ResolveProvider(key string) (SubProvider, bool) {
	switch key {
	case "opencode", "hermes", "codex", "poolside", "freebuff":
	default:
		return SubProvider{}, false
	}

	cfg, _ := Load()
	p, ok := cfg.Subscriptions.Providers[key]
	if !ok {
		p = SubProvider{Kind: key}
	}

	// Env prefix: SOORAA_OPENCODE_, SOORAA_HERMES_, SOORAA_CODEX_
	envPrefix := "SOORAA_" + upper(key) + "_"

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
	case "poolside":
		candidates = []string{
			"poolside",
			filepath.Join(home, ".local", "bin", "poolside"),
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
	case "poolside":
		return filepath.Join(home, ".config", "poolside")
	case "freebuff":
		// The Freebuff CLI keeps credentials in ~/.config/manicode. Pointing
		// AuthDir there lets the read-only import find an existing CLI login.
		return filepath.Join(home, ".config", "manicode")
	default:
		return ""
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
