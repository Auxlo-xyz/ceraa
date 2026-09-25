package operation

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var mutationLocks struct {
	sync.Mutex
	items map[string]*sync.Mutex
}

func init() {
	mutationLocks.items = make(map[string]*sync.Mutex)
}

func withMutationLock(path string, fn func() error) error {
	key, err := canonicalMutationKey(path)
	if err != nil {
		return err
	}
	mutationLocks.Lock()
	lock := mutationLocks.items[key]
	if lock == nil {
		lock = &sync.Mutex{}
		mutationLocks.items[key] = lock
	}
	mutationLocks.Unlock()
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

func canonicalMutationKey(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("prepare mutation path: %w", err)
	}
	return abs, nil
}
