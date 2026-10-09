package config

import (
	"path/filepath"
	"sync"
	"testing"
)

// Two writers, one file: the daemon and the CLI edit config.json concurrently,
// and two daemons can hold the same token and the same file. An in-memory mutex
// cannot serialise writers in different address spaces, so concurrent
// read-modify-write cycles used to silently discard one side's change. That is
// how a provider switch confirmed on screen never took effect for the next run.
// Separate Store instances stand in for separate processes here: they share no
// mutex, only the file lock.
func TestConcurrentStoresDoNotLoseUpdates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := NewStoreAt(path)
			name := string(rune('a' + i))
			if err := store.Update(func(c *Config) error {
				if c.ProviderProfiles == nil {
					c.ProviderProfiles = map[string]ProviderProfile{}
				}
				c.ProviderProfiles[name] = ProviderProfile{Model: "model-" + name}
				return nil
			}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent update: %v", err)
	}
	got, err := NewStoreAt(path).Load()
	if err != nil {
		t.Fatalf("load after concurrent updates: %v", err)
	}
	for i := 0; i < writers; i++ {
		name := string(rune('a' + i))
		if _, ok := got.ProviderProfiles[name]; !ok {
			t.Errorf("profile %q lost: a concurrent update discarded it", name)
		}
	}
}
