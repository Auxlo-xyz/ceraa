package whatsapp

import (
	"path/filepath"
	"strconv"
	"strings"

	"ceraa/internal/envs"
	"ceraa/internal/paths"
)

// Config carries the Ceraa chat settings. Every value comes from the
// environment; there is no config file section for this package.
type Config struct {
	// Enabled gates Connect. False by default so a stray install never
	// dials out on its own.
	Enabled bool
	// DBFile overrides the session database path. Empty means the default
	// under the Ceraa data dir.
	DBFile string
	// AllowJIDs is the owner allow-list. See Allowed for the empty case.
	AllowJIDs []string
}

// LoadConfig reads the CERAA_WHATSAPP_* variables.
func LoadConfig() Config {
	enabled := false
	if raw := strings.TrimSpace(envs.Get("WHATSAPP_ENABLED")); raw != "" {
		if ok, err := strconv.ParseBool(raw); err == nil {
			enabled = ok
		}
	}
	return Config{
		Enabled:   enabled,
		DBFile:    strings.TrimSpace(envs.Get("WHATSAPP_DB_FILE")),
		AllowJIDs: parseAllowList(envs.Get("WHATSAPP_ALLOW_JIDS")),
	}
}

// DBPath resolves the session database file. The env override wins;
// otherwise it is whatsapp.db inside the Ceraa data dir. A relative
// override resolves against the data dir, not the working directory.
func (c Config) DBPath() string {
	if c.DBFile == "" {
		return filepath.Join(paths.DataDir(), "whatsapp.db")
	}
	if filepath.IsAbs(c.DBFile) {
		return c.DBFile
	}
	return filepath.Join(paths.DataDir(), c.DBFile)
}

// Allowed reports whether senderJID may use the bridge. An empty
// allow-list is owner-only mode: only the linked account itself passes.
// In practice self-messages never reach this check (the bridge skips
// FromMe first), so pair the empty list with the allow-list below in
// real installs: put your own number in the list, e.g.
// CERAA_WHATSAPP_ALLOW_JIDS=2348012345678@s.whatsapp.net. An unknown
// self ("" ) fails closed. The transport itself does not filter; the
// bridge calls this per message.
func (c Config) Allowed(senderJID, selfJID string) bool {
	if len(c.AllowJIDs) == 0 {
		return selfJID != "" && normalizeJID(senderJID) == normalizeJID(selfJID)
	}
	want := normalizeJID(senderJID)
	for _, entry := range c.AllowJIDs {
		if normalizeJID(entry) == want {
			return true
		}
	}
	// Server-suffix tolerance: a listed phone number matches the same
	// digits on any server (@s.whatsapp.net vs @lid), so a server-side
	// identity migration cannot lock the owner out. Digits only, so a
	// group or broadcast id never matches a phone entry.
	return digitsMatch(c.AllowJIDs, senderJID)
}

// digitsMatch reports whether any allow entry carries the same digit
// string as the sender's user part.
func digitsMatch(entries []string, senderJID string) bool {
	want := jidDigits(senderJID)
	if want == "" {
		return false
	}
	for _, entry := range entries {
		if jidDigits(entry) == want {
			return true
		}
	}
	return false
}

// jidDigits keeps only the digits of the user part before the @.
func jidDigits(raw string) string {
	user := strings.TrimSpace(raw)
	if at := strings.Index(user, "@"); at >= 0 {
		user = user[:at]
	}
	var b strings.Builder
	for _, r := range user {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// parseAllowList splits the comma-separated env value, dropping blanks.
func parseAllowList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// normalizeJID folds a JID to user@server so a device suffix never
// decides an allow-list comparison.
func normalizeJID(raw string) string {
	raw = strings.TrimSpace(raw)
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		user := raw[:at]
		if dot := strings.Index(user, "."); dot >= 0 {
			user = user[:dot]
		}
		return user + raw[at:]
	}
	return raw
}
