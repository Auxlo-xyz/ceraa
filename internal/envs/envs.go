// Package envs reads Ceraa settings from the environment.
//
// Ceraa shipped under the name Sooraa, so both prefixes are accepted: CERAA_ is
// canonical and SOORAA_ is a fallback. A CERAA_ value always wins when both are
// set. The rule lives in one place on purpose: it is duplicated across the
// harness and the internal packages, and a rename is exactly the kind of change
// that drifts when four copies each remember the old prefix.
//
// The lookup is not cached. An operator who renames a variable should not have
// to restart to see it take effect, and a per-call read costs nothing next to
// the network call it configures.
package envs

import "os"

// LegacyPrefix is the pre-rename variable prefix, still honoured.
const LegacyPrefix = "SOORAA_"

// Prefix is the canonical variable prefix.
const Prefix = "CERAA_"

// Get returns the value of a Ceraa setting by its suffix, e.g. Get("API_KEY")
// reads CERAA_API_KEY and falls back to SOORAA_API_KEY.
func Get(name string) string {
	if value := os.Getenv(Prefix + name); value != "" {
		return value
	}
	return os.Getenv(LegacyPrefix + name)
}

// GetWith returns the value of the first non-empty variable among the supplied
// full names, in order. Used for settings that also have a third-party name.
func GetWith(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

// Set writes the canonical prefix so anything reading the setting later sees a
// Ceraa-native value rather than one under the legacy name.
func Set(name, value string) error {
	return os.Setenv(Prefix+name, value)
}
