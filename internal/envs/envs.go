// Package envs reads Ceraa settings from the environment.
//
// One prefix, no compatibility shim: CERAA_ is the only supported name. The
// lookup is not cached, so changing a variable takes effect without a restart,
// and a per-call read costs nothing next to the network call it configures.
package envs

import "os"

// Prefix is the variable prefix for every Ceraa setting.
const Prefix = "CERAA_"

// Get returns the value of a Ceraa setting by its suffix, e.g. Get("API_KEY")
// reads CERAA_API_KEY.
func Get(name string) string {
	return os.Getenv(Prefix + name)
}

// GetWith returns the value of the first non-empty variable among the supplied
// full names, in order. Used for settings that also have a third-party name,
// such as GEMINI_API_KEY.
func GetWith(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

// Set writes a Ceraa setting.
func Set(name, value string) error {
	return os.Setenv(Prefix+name, value)
}
