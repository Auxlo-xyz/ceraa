// Package secret provides encrypted storage for credentials entered via
// Telegram, so secrets can be passed to agent child processes without being
// persisted in plaintext. Uses AES-256-GCM at rest.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is a single encrypted secret.
type Entry struct {
	Encrypted []byte `json:"encrypted"`
	Nonce     []byte `json:"nonce"`
	CreatedAt int64  `json:"created_at"`
	// Internal marks a credential Ceraa resolves for itself. It is absent on
	// entries written before the field existed, which is why IsInternal still
	// classifies those by name prefix: the flag settles the new writes, and the
	// prefixes settle the old ones.
	Internal bool `json:"internal,omitempty"`
}

// store is the on-disk secret store.
type store struct {
	path    string
	key     []byte // 32-byte AES key
	entries map[string]Entry
}

// mu guards the store pointer and everything behind it.
//
// Two parts of one process reach this map and neither knows about the other:
// the chat handlers, and the loopback surface. A map written from both is a
// lost write or a fatal concurrent read, so the lock lives at the boundary
// rather than being left to whichever caller happened to be running.
var mu sync.RWMutex

var defaultStore *store

// Init initializes the default secret store.
// keyFile is the file containing the 32-byte encryption key.
// If keyFile doesn't exist, it's created with random key.
func Init(keyFile, secretFile string) error {
	key, err := loadOrCreateKey(keyFile)
	if err != nil {
		return fmt.Errorf("secret store key: %w", err)
	}

	loaded := make(map[string]Entry)
	data, err := os.ReadFile(secretFile)
	if err == nil {
		var persisted map[string]Entry
		if err := json.Unmarshal(data, &persisted); err == nil {
			loaded = persisted
		}
	}

	mu.Lock()
	defaultStore = &store{
		path:    secretFile,
		key:     key,
		entries: loaded,
	}
	mu.Unlock()

	return nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	// Try loading existing key
	if data, err := os.ReadFile(path); err == nil {
		if len(data) >= 32 {
			return data[:32], nil
		}
	}

	// Create directory and generate new key
	if dir := filepath.Dir(path); dir != "" {
		os.MkdirAll(dir, 0o700)
	}

	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	// Store key file with restrictive permissions
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("write key: %w", err)
	}

	return key, nil
}

// Set encrypts and stores a credential Ceraa resolves for itself.
//
// Writes are internal unless a surface says otherwise, because the store cannot
// tell one kind from the other by looking at the name alone. A provider that
// lands a new credential outside the reserved prefixes used to be exposed the
// moment it was stored; now the exposure takes an explicit SetUser, which only
// the surfaces a person owns call.
func Set(name, value string) error { return set(name, value, true) }

// SetUser encrypts and stores a secret the person owns, for display and for
// offering to the agent by name.
//
// A reserved name is refused rather than stored under the wrong owner: Ceraa's
// own provider tokens and bot tokens live at those names, and a surface that
// could write one could replace the credential the process authenticates with.
func SetUser(name, value string) error {
	if IsInternal(name) {
		return fmt.Errorf("secret %q is reserved by Ceraa", name)
	}
	return set(name, value, false)
}

func set(name, value string, internal bool) error {
	if name == "" {
		return fmt.Errorf("secret name required")
	}

	key, err := storeKey()
	if err != nil {
		return err
	}
	// The key is read under its own short lock and the write taken after, so a
	// slow encrypt does not sit on the lock a reader is waiting for. The store
	// can only have been closed underneath us in that window, and if it was,
	// the entry belongs nowhere.
	encrypted, nonce, err := encrypt(key, value)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if defaultStore == nil {
		return fmt.Errorf("secret store not initialized")
	}

	defaultStore.entries[name] = Entry{
		Encrypted: encrypted,
		Nonce:     nonce,
		CreatedAt: time.Now().Unix(),
		Internal:  internal,
	}

	// save reads the map it is handed, so it runs under the write lock rather
	// than taking its own: two entries written together must be the same bytes
	// on disk as the ones in memory when the file is closed.
	return defaultStore.save()
}

// Get decrypts and returns a secret.
func Get(name string) (string, error) {
	mu.RLock()
	defer mu.RUnlock()
	if defaultStore == nil {
		return "", fmt.Errorf("secret store not initialized")
	}

	entry, ok := defaultStore.entries[name]
	if !ok {
		return "", fmt.Errorf("secret %q not found", name)
	}

	value, err := decrypt(defaultStore.key, entry.Encrypted, entry.Nonce)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return value, nil
}

// internalPrefixes mark credentials the harness resolves for itself.
//
// The store holds two kinds of secret in one file: the ones a person set for the
// agent to use, and Ceraa's own provider tokens. Both are credentials, and the
// second kind must not be reachable by anything the model can call — a tool that
// can read `ceraa_minimax_global` can read the account, and one that could read a
// bot token would own the host. The code that already has them keeps having them.
var internalPrefixes = []string{
	"ceraa_",
	// The OAuth providers whose credentials do not ride the ceraa_ namespace.
	// Each of these was a live exposure until it was named here: gemini_,
	// qoder_ and workbuddy_ were readable through list_secrets, which is the
	// reason writes are internal by default now.
	"gemini_",
	"qoder_",
	"workbuddy_",
	"buddy_",
	// Provider onboarding writes this name (internal/telegram/commands.go), and the
	// agent bridge reads it back to authenticate every request. It was not reserved,
	// so a provider key stored this way was a user secret: list_secrets named it and
	// run_command_with_secret would inject it into a command of the model's
	// choosing. The prefix list reads as if it were enumerated from the credentials
	// that existed when it was written, and a writer that lands outside it does so
	// silently — so this one is reserved by the name onboarding actually uses.
	"provider_api_key_",
	"codex_",
	"hermes_",
	"xai_",
	"cline_",
	"minimax_",
}

// ReservedPrefixes returns the prefixes Ceraa keeps for itself, for surfaces
// that validate names before sending them. A copy quoted in a UI would drift
// from this list without anyone noticing, so the dashboard asks instead.
func ReservedPrefixes() []string {
	out := append([]string(nil), internalPrefixes...)
	return out
}

// IsInternal reports whether a name is Ceraa's own rather than a user's.
//
// Two answers, because the store has two generations in it. An entry written
// since Set defaulted to internal carries the flag, and that flag is the truth
// for it. An entry written before then has no flag, so the name is matched
// against the prefixes: the same list a surface validates against, kept for the
// credentials that were already on disk when the flag arrived.
func IsInternal(name string) bool {
	mu.RLock()
	defer mu.RUnlock()
	return isInternalLocked(name)
}

func isInternalLocked(name string) bool {
	for _, prefix := range internalPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	if defaultStore == nil {
		return false
	}
	return defaultStore.entries[name].Internal
}

// ListUser returns the stored names a person set, for display and for offering as
// choices. Ceraa's own credentials are excluded rather than shown, since a menu
// that lists them invites picking one.
func ListUser() []string {
	mu.RLock()
	defer mu.RUnlock()
	if defaultStore == nil {
		return nil
	}
	out := make([]string, 0, len(defaultStore.entries))
	for name := range defaultStore.entries {
		if !isInternalLocked(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// List returns all stored secret names.
func List() []string {
	mu.RLock()
	defer mu.RUnlock()
	if defaultStore == nil {
		return nil
	}
	names := make([]string, 0, len(defaultStore.entries))
	for name := range defaultStore.entries {
		names = append(names, name)
	}
	return names
}

// Delete removes a secret.
func Delete(name string) error {
	mu.Lock()
	defer mu.Unlock()
	if defaultStore == nil {
		return fmt.Errorf("secret store not initialized")
	}
	delete(defaultStore.entries, name)
	return defaultStore.save()
}

// storeKey returns the encryption key for a caller about to encrypt. It takes
// the read lock for the length of one map lookup and nothing longer.
func storeKey() ([]byte, error) {
	mu.RLock()
	defer mu.RUnlock()
	if defaultStore == nil {
		return nil, fmt.Errorf("secret store not initialized")
	}
	return defaultStore.key, nil
}

func (s *store) save() error {
	data, err := json.Marshal(s.entries)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(s.path, data, 0o600)
}

func encrypt(key []byte, plaintext string) (encrypted, nonce []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}

	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}

	encrypted = gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return encrypted, nonce, nil
}

func decrypt(key, encrypted, nonce []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	plaintext, err := gcm.Open(nil, nonce, encrypted, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}
