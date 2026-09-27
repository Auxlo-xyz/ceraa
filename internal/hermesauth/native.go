package hermesauth

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ceraa/internal/secret"
)

// nativeSecretPrefix namespaces native Hermes credentials in the encrypted store.
const nativeSecretPrefix = "ceraa_native_hermes_"

func nativeSecretName(providerID string) string {
	return nativeSecretPrefix + normalizeProvider(providerID)
}

func (s *Store) loadNative() map[string]ProviderState {
	result := map[string]ProviderState{}
	for _, providerID := range []string{"nous"} {
		value, err := secret.Get(nativeSecretName(providerID))
		if err != nil || strings.TrimSpace(value) == "" {
			continue
		}
		var record nativeRecord
		if json.Unmarshal([]byte(value), &record) != nil {
			continue
		}
		result[providerID] = ProviderState{
			AccessToken:       record.AccessToken,
			RefreshToken:      record.RefreshToken,
			AgentKey:          record.AgentKey,
			AgentKeyExpiresAt: record.AgentKeyExpiresAt,
			BaseURL:           record.BaseURL,
			InferenceBaseURL:  record.BaseURL,
			PortalBaseURL:     record.PortalBaseURL,
			ClientID:          record.ClientID,
			TokenEndpoint:     record.TokenEndpoint,
			TokenType:         record.TokenType,
			ExpiresAt:         record.ExpiresAt,
		}
	}
	return result
}

func (s *Store) saveNative(credential Credential) error {
	providerID := normalizeProvider(credential.Provider)
	record := nativeRecord{
		Provider:      providerID,
		AccessToken:   credential.AccessToken,
		RefreshToken:  credential.RefreshToken,
		BaseURL:       credential.BaseURL,
		PortalBaseURL: credential.PortalBaseURL,
		ClientID:      credential.ClientID,
		TokenEndpoint: credential.TokenEndpoint,
		TokenType:     credential.TokenType,
		ExpiresAt:     credential.ExpiresAt,
		Scope:         "inference:invoke",
	}
	if providerID == "nous" {
		record.AgentKey = credential.AccessToken
		record.AgentKeyExpiresAt = credential.ExpiresAt
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode native hermes credential: %w", err)
	}
	if err := secret.Set(nativeSecretName(providerID), string(data)); err != nil {
		return fmt.Errorf("save native hermes credential: %w", err)
	}
	return nil
}

func nativeCredentialExpiry(credential Credential) time.Time {
	if credential.ExpiresAt.IsZero() {
		return time.Now().Add(15 * time.Minute)
	}
	return credential.ExpiresAt
}

// SignedIn reports whether a usable Hermes credential is already stored.
//
// Used by the Telegram menu to decide between "sign in" and "switch to it". A
// stored-but-unusable credential must not read as signed in, so this resolves the
// credential the same way an inference request would rather than checking for a
// file that exists.
func SignedIn() bool {
	store := NewStore()
	snapshot, err := store.Load()
	if err != nil {
		return false
	}
	if _, err := resolveFromSnapshot(snapshot, normalizeProvider(snapshot.ActiveProvider)); err == nil {
		return true
	}
	// ActiveProvider can be empty in a store that still holds a usable Nous
	// credential, which is what a device login through this package writes.
	if _, err := resolveFromSnapshot(snapshot, "nous"); err == nil {
		return true
	}
	return false
}
