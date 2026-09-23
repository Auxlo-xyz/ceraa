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
	"time"
)

// Entry is a single encrypted secret.
type Entry struct {
	Encrypted []byte `json:"encrypted"`
	Nonce     []byte `json:"nonce"`
	CreatedAt int64  `json:"created_at"`
}

// store is the on-disk secret store.
type store struct {
	path     string
	key      []byte // 32-byte AES key
	entries  map[string]Entry
}

var defaultStore *store

// Init initializes the default secret store.
// keyFile is the file containing the 32-byte encryption key.
// If keyFile doesn't exist, it's created with random key.
func Init(keyFile, secretFile string) error {
	key, err := loadOrCreateKey(keyFile)
	if err != nil {
		return fmt.Errorf("secret store key: %w", err)
	}

	defaultStore = &store{
		path:    secretFile,
		key:     key,
		entries: make(map[string]Entry),
	}

	// Load existing entries
	data, err := os.ReadFile(secretFile)
	if err == nil {
		var persisted map[string]Entry
		if err := json.Unmarshal(data, &persisted); err == nil {
			defaultStore.entries = persisted
		}
	}

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

// Set encrypts and stores a secret.
func Set(name, value string) error {
	if defaultStore == nil {
		return fmt.Errorf("secret store not initialized")
	}
	if name == "" {
		return fmt.Errorf("secret name required")
	}

	encrypted, nonce, err := encrypt(defaultStore.key, value)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}

	defaultStore.entries[name] = Entry{
		Encrypted: encrypted,
		Nonce:     nonce,
		CreatedAt: time.Now().Unix(),
	}

	return defaultStore.save()
}

// Get decrypts and returns a secret.
func Get(name string) (string, error) {
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

// List returns all stored secret names.
func List() []string {
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
	if defaultStore == nil {
		return fmt.Errorf("secret store not initialized")
	}
	delete(defaultStore.entries, name)
	return defaultStore.save()
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
