package freebuff

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ceraa/internal/secret"
)

// nativeSecretName is the single encrypted entry holding the whole Freebuff
// CLI "default" profile. Storing the entire profile (rather than just the
// token) means logout is one secret.Delete and fingerprintId/fingerprintHash
// round-trip unchanged.
// nativeSecretName is the encrypted-store key for the single Freebuff profile.
const nativeSecretName = "ceraa_native_freebuff_default"

// ErrNoCredential is returned when no Freebuff credential is available.
var ErrNoCredential = errors.New("freebuff credential not found")

// User mirrors the Freebuff CLI credentials.json "default" profile. Name is
// intentionally a plain string: the CLI coerces null to "" because requiring
// name to be a string logged every null-name account out on every launch.
type User struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Email           string  `json:"email"`
	AuthToken       string  `json:"authToken"`
	FingerprintID   string  `json:"fingerprintId"`
	FingerprintHash string  `json:"fingerprintHash"`
	Credits         float64 `json:"credits,omitempty"`
}

// cliCredentialsFile is the Freebuff CLI on-disk format: a map of profile name
// to profile, of which only "default" matters to us.
type cliCredentialsFile map[string]cliProfile

type cliProfile struct {
	ID              string  `json:"id"`
	Name            *string `json:"name"` // nullable in the wild
	Email           string  `json:"email"`
	AuthToken       string  `json:"authToken"`
	FingerprintID   string  `json:"fingerprintId"`
	FingerprintHash string  `json:"fingerprintHash"`
	Credits         float64 `json:"credits"`
}

// cliConfigDir resolves the Freebuff CLI config directory. FREEBUFF_CONFIG_DIR
// wins if set and absolute; otherwise ~/.config/manicode (+ a dev/test suffix).
func cliConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("FREEBUFF_CONFIG_DIR")); dir != "" && filepath.IsAbs(dir) {
		return dir
	}
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".config", "manicode")
	if env := strings.TrimSpace(os.Getenv("NEXT_PUBLIC_CB_ENVIRONMENT")); env != "" && env != "prod" {
		return base + "-" + env
	}
	return base
}

// importFromCLI is a best-effort, read-only read of the Freebuff CLI
// credentials file. It never writes that file: two writers on one file is a
// guaranteed clobber, and Ceraa's whole value proposition is the encrypted
// store. Returns (nil, nil) when no CLI credentials exist.
func importFromCLI() (*User, error) {
	path := filepath.Join(cliConfigDir(), "credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, nil // unreadable is treated as absent; never blocks login
	}
	var file cliCredentialsFile
	if json.Unmarshal(data, &file) != nil {
		return nil, nil
	}
	profile, ok := file["default"]
	if !ok {
		return nil, nil
	}
	name := ""
	if profile.Name != nil {
		name = *profile.Name
	}
	if strings.TrimSpace(profile.AuthToken) == "" {
		return nil, nil
	}
	return &User{
		ID:              profile.ID,
		Name:            name,
		Email:           profile.Email,
		AuthToken:       profile.AuthToken,
		FingerprintID:   profile.FingerprintID,
		FingerprintHash: profile.FingerprintHash,
		Credits:         profile.Credits,
	}, nil
}

// saveUser encrypts and stores the Freebuff profile in Ceraa's secret store.
func saveUser(user User) error {
	if strings.TrimSpace(user.AuthToken) == "" {
		return errors.New("freebuff user has empty authToken")
	}
	if user.FingerprintID == "" {
		user.FingerprintID = newFingerprintID()
	}
	data, err := json.Marshal(user)
	if err != nil {
		return fmt.Errorf("encode freebuff user: %w", err)
	}
	if err := secret.Set(nativeSecretName, string(data)); err != nil {
		return fmt.Errorf("save freebuff credential: %w", err)
	}
	return nil
}

// loadUser resolves the Freebuff profile: Ceraa's encrypted store first, then
// a read-only import from the Freebuff CLI. The returned User is always safe to
// persist (an imported fingerprintID, if present, is kept verbatim so the
// server sees one device rather than two).
func loadUser() (*User, error) {
	if value, err := secret.Get(nativeSecretName); err == nil && strings.TrimSpace(value) != "" {
		var user User
		if json.Unmarshal([]byte(value), &user) == nil && strings.TrimSpace(user.AuthToken) != "" {
			if user.FingerprintID == "" {
				user.FingerprintID = newFingerprintID()
			}
			return &user, nil
		}
	}
	if imported, err := importFromCLI(); err == nil && imported != nil {
		return imported, nil
	}
	return nil, ErrNoCredential
}

// clearUser removes the encrypted Freebuff credential. Idempotent: secret.Delete
// only errors when the store is uninitialized.
func clearUser() error {
	return secret.Delete(nativeSecretName)
}
