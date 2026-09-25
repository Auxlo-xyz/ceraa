package freebuff

import (
	"context"
	"fmt"
	"strings"
)

// Credential is the resolved Freebuff credential for inference: the bearer
// token plus the hosted API base URL and the identity fields.
type Credential struct {
	AccessToken string
	BaseURL     string
	User        User
	Source      string
}

// Store resolves Freebuff credentials for the agent and CLI paths. It exists
// to mirror hermesauth.Store so the wiring in agent-bridge.go and agent.go
// reads the same way.
type Store struct {
	// APIBaseURL overrides the hosted API host. Left empty in production so the
	// compile-time constant is used; tests set it to a local server.
	APIBaseURL string
	Client     *Client
}

func NewStore() *Store { return &Store{} }

// Resolve returns the current Freebuff credential. Precedence: the Ceraa
// encrypted store, then a read-only import from the Freebuff CLI.
func (s *Store) Resolve(ctx context.Context) (Credential, error) {
	user, err := loadUser()
	if err != nil {
		return Credential{}, err
	}
	credential := Credential{
		AccessToken: user.AuthToken,
		BaseURL:     DefaultInferenceBaseURL,
		User:        *user,
		Source:      "freebuff-native",
	}
	// A test override targets the API host; the inference base is derived from
	// it so both the /me check and completions stay on the same server.
	if s != nil && strings.TrimSpace(s.APIBaseURL) != "" {
		credential.BaseURL = strings.TrimRight(s.APIBaseURL, "/") + "/api/v1"
	}
	return credential, nil
}

// Status reports the identity of the stored credential, validating it live
// against /api/v1/me. An authentication failure (401/403/404) clears the stored
// credential so the user is told to log in again; a transient error is surfaced
// to the caller but never clears local state.
func (s *Store) Status(ctx context.Context) (string, error) {
	user, err := loadUser()
	if err != nil {
		return "", err
	}
	identity := identityOf(user)
	client := s.client()
	me, meErr := client.Me(ctx, user.AuthToken)
	if meErr != nil {
		if MeAuthError(meErr) {
			_ = clearUser()
			return identity, fmt.Errorf("stored Freebuff token was rejected; run /freebuff-login again")
		}
		// Transient: keep the credential and tell the user the check failed.
		return identity, fmt.Errorf("could not verify the Freebuff session: %w", meErr)
	}
	if me != nil {
		if me.Email != "" {
			user.Email = me.Email
		}
		if me.ID != "" {
			user.ID = me.ID
		}
	}
	return identityOf(user), nil
}

func identityOf(user *User) string {
	if user.Email != "" {
		return user.Email
	}
	if user.ID != "" {
		return user.ID
	}
	return "unknown account"
}

// Logout clears the local credential and best-effort invalidates the remote
// session. Like the Freebuff CLI, a failed remote call never blocks the local
// clear and never surfaces an error: logout always succeeds.
func (s *Store) Logout(ctx context.Context) error {
	user, err := loadUser()
	if err != nil {
		return nil // idempotent when not logged in
	}
	_ = s.client().Logout(ctx, user.AuthToken, *user)
	return clearUser()
}

func (s *Store) client() *Client {
	if s != nil && s.Client != nil {
		return s.Client
	}
	if s != nil && strings.TrimSpace(s.APIBaseURL) != "" {
		return &Client{HTTPClient: nil, WebBaseURL: DefaultWebBaseURL, APIBaseURL: s.APIBaseURL}
	}
	return NewClient()
}
