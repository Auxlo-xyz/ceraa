package opencodeauth

import "ceraa/internal/secret"

func secretInit(key, file string) error { return secret.Init(key, file) }
func secretSet(name, value string) error {
	return secret.Set(name, value)
}
