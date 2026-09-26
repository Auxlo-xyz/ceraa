package llm

import "sync"

// resetMediaSupportForTest clears the in-process cache so a test starts from
// nothing, and forgets the resolver.
func resetMediaSupportForTest() {
	mediaSupportMu.Lock()
	defer mediaSupportMu.Unlock()
	mediaSupportLoaded = false
	mediaSupportRefusals = nil
	mediaSupportHostOfKey = func(string) string { return "" }
}

var _ sync.Mutex
