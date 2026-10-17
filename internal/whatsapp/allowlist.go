package whatsapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ceraa/internal/envs"
	"ceraa/internal/paths"
)

// The allow-list has two halves that answer one question and are stored
// separately on purpose. The environment seed is the operator's: it is set
// before the daemon starts and the hub never edits it. The stored half is the
// owner's: the hub adds and removes entries at runtime. Both are honored, so a
// number the operator locked in cannot be dropped from a chat.
//
// The store exists because the environment was the only place an allowed
// number could live, which meant a person who had just paired a phone had to
// edit an env file and restart before their own messages did anything. The
// failure was silent: the bridge left an empty allow-list as owner-only, and
// the owner's own messages are skipped as FromMe, so nobody was served and
// nothing said why.

// allowListPath is indirected so a test can point it at a temp file without
// setting CERAA_HOME, which paths.DataDir caches and would leak between tests.
// Mirrors the statusPath pattern in status.go.
var allowListPath = ""

// AllowListPath is the file holding the hub-editable allow-list.
func AllowListPath() string {
	if allowListPath != "" {
		return allowListPath
	}
	return filepath.Join(paths.DataDir(), "whatsapp-allowlist.json")
}

// SetAllowListPathForTest pins the allow-list file for a test and returns a
// restore function. Kept in the shipped package rather than a _test file
// because the menus tests, in another package, need it.
func SetAllowListPathForTest(path string) func() {
	previous := allowListPath
	allowListPath = path
	return func() { allowListPath = previous }
}

// allowFile is the on-disk shape. A struct rather than a bare array so the file
// can grow a field without a migration.
type allowFile struct {
	JIDs []string `json:"jids"`
}

// LoadAllowList reads the hub-editable entries. A missing or unreadable file
// reads as empty, never an error: the environment seed and the owner-only
// fallback still apply, and a corrupt store must not lock the owner out.
func LoadAllowList() []string {
	data, err := os.ReadFile(AllowListPath())
	if err != nil {
		return nil
	}
	var stored allowFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil
	}
	out := make([]string, 0, len(stored.JIDs))
	for _, entry := range stored.JIDs {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// SaveAllowList writes the stored entries atomically. A write failure is
// returned rather than panicked: the worst case is an entry the hub reported as
// added being absent after a restart.
func SaveAllowList(jids []string) error {
	path := AllowListPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	payload, err := json.Marshal(allowFile{JIDs: jids})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".whatsapp-allowlist-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = os.Remove(tmp.Name())
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ErrNotAPhoneNumber marks a value the allow-list cannot store. The caller
// shows it verbatim, so the wording has to read as advice.
var ErrNotAPhoneNumber = errors.New("that is not a phone number")

// normalizeAllowEntry validates a typed number and returns its canonical
// digits-only form. Formatting (a leading +, spaces, dashes, dots, parentheses)
// is accepted and dropped; any other character is refused, so a JID, a group id
// or a stray word is rejected rather than stored as an entry that never
// matches. The length window matches the pairing validator, one number shape
// for the whole surface.
func normalizeAllowEntry(raw string) (string, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "+"))
	var digits strings.Builder
	for _, r := range trimmed {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
			// Formatting only; dropped from the stored entry.
		default:
			return "", fmt.Errorf("%w: %q is not a digit", ErrNotAPhoneNumber, string(r))
		}
	}
	out := digits.String()
	if out == "" {
		return "", ErrNotAPhoneNumber
	}
	if out[0] == '0' {
		return "", fmt.Errorf("%w: drop the leading 0 and use the country code", ErrNotAPhoneNumber)
	}
	if len(out) < minPhoneDigits {
		return "", fmt.Errorf("%w: expected at least %d digits, got %d", ErrNotAPhoneNumber, minPhoneDigits, len(out))
	}
	if len(out) > maxPhoneDigits {
		return "", fmt.Errorf("%w: expected at most %d digits, got %d", ErrNotAPhoneNumber, maxPhoneDigits, len(out))
	}
	return out, nil
}

// AddAllowListEntry validates and stores one number, deduping against every
// existing entry by digits. It reports whether the entry was new, so the caller
// can say "added" rather than "already allowed".
func AddAllowListEntry(raw string) (entry string, added bool, err error) {
	entry, err = normalizeAllowEntry(raw)
	if err != nil {
		return "", false, err
	}
	existing := LoadAllowList()
	for _, have := range existing {
		if jidDigits(have) == entry {
			return entry, false, nil
		}
	}
	if err := SaveAllowList(append(existing, entry)); err != nil {
		return "", false, err
	}
	return entry, true, nil
}

// RemoveAllowListEntry drops the stored entry matching raw by digits. It only
// ever touches the stored half: a number the operator set in the environment
// stays allowed, which is the point of keeping the two apart. It reports whether
// anything was removed.
func RemoveAllowListEntry(raw string) (bool, error) {
	want := jidDigits(raw)
	if want == "" {
		return false, nil
	}
	existing := LoadAllowList()
	kept := make([]string, 0, len(existing))
	removed := false
	for _, have := range existing {
		if jidDigits(have) == want {
			removed = true
			continue
		}
		kept = append(kept, have)
	}
	if !removed {
		return false, nil
	}
	return true, SaveAllowList(kept)
}

// EnvAllowList is the operator's half: the comma-separated
// CERAA_WHATSAPP_ALLOW_JIDS value, blanks dropped.
func EnvAllowList() []string {
	return parseAllowList(envs.Get("WHATSAPP_ALLOW_JIDS"))
}

// EffectiveAllowList is the environment seed followed by the stored entries,
// deduped by digits, in order. This is the list the bridge actually enforces.
func EffectiveAllowList() []string {
	env := EnvAllowList()
	stored := LoadAllowList()
	out := make([]string, 0, len(env)+len(stored))
	seen := make(map[string]bool, len(env)+len(stored))
	for _, entry := range append(env, stored...) {
		key := jidDigits(entry)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, entry)
	}
	return out
}

// Allowed reports whether senderJID may use the bridge, honoring the
// environment seed and the hub-editable store. Same empty-list semantics as
// Config.Allowed: an empty list is owner-only, and the bridge skips the owner's
// own messages before this runs, so with nothing listed nobody is served.
func Allowed(senderJID, selfJID string) bool {
	return Config{AllowJIDs: EffectiveAllowList()}.Allowed(senderJID, selfJID)
}

// SameNumber reports whether two values carry the same digit string. Used to
// warn when the owner adds the linked account itself, which the bridge skips.
func SameNumber(a, b string) bool {
	left := jidDigits(a)
	return left != "" && left == jidDigits(b)
}
