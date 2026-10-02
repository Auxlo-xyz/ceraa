package secret

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestInitAndSetGetDelete(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "secret.key")
	secretFile := filepath.Join(dir, "secrets.json")

	// Override default paths for test
	// Override HOME for any future tests, though we use explicit paths
	t.Setenv("HOME", dir)

	if err := Init(keyFile, secretFile); err != nil {
		t.Fatal(err)
	}

	// Set a secret
	if err := Set("test-key", "sk-12345678901234567890"); err != nil {
		t.Fatal(err)
	}

	// Get the secret
	val, err := Get("test-key")
	if err != nil {
		t.Fatal(err)
	}
	if val != "sk-12345678901234567890" {
		t.Errorf("expected secret value, got %q", val)
	}

	// Verify it's not stored in plaintext
	data, _ := os.ReadFile(secretFile)
	plaintext := string(data)
	if containsPlaintext(plaintext, "sk-12345678901234567890") {
		t.Error("secret stored in plaintext")
	}

	// List should show the key
	names := List()
	if len(names) != 1 || names[0] != "test-key" {
		t.Errorf("expected [test-key], got %v", names)
	}

	// Delete
	if err := Delete("test-key"); err != nil {
		t.Fatal(err)
	}

	// Verify deleted
	_, err = Get("test-key")
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestEmptyStore(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "secret.key")
	secretFile := filepath.Join(dir, "secrets.json")

	t.Setenv("HOME", dir)

	if err := Init(keyFile, secretFile); err != nil {
		t.Fatal(err)
	}

	_, err := Get("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent secret")
	}

	names := List()
	if len(names) != 0 {
		t.Errorf("expected empty list, got %v", names)
	}
}

func containsPlaintext(s, secret string) bool {
	for i := 0; i <= len(s)-len(secret); i++ {
		if s[i:i+len(secret)] == secret {
			return true
		}
	}
	return false
}

// TestConcurrentWritersDoNotLoseAnEntry is the guard for two surfaces touching
// the same map. Without a lock this either loses a write or the runtime ends the
// process on a concurrent map access, and the second is the one a reader notices.
func TestConcurrentWritersDoNotLoseAnEntry(t *testing.T) {
	dir := t.TempDir()
	if err := Init(filepath.Join(dir, "key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}

	const writers = 16
	const perWriter = 24
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				name := fmt.Sprintf("k-%d-%d", w, i)
				if err := Set(name, "v"); err != nil {
					errs <- err
					return
				}
				if _, err := Get(name); err != nil {
					errs <- err
					return
				}
				List()
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	got := List()
	if len(got) != writers*perWriter {
		t.Fatalf("stored %d names, want %d: a concurrent write was lost", len(got), writers*perWriter)
	}
}
