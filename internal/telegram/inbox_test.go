package telegram

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestInboxDeduplication(t *testing.T) {
	tempDir := t.TempDir()
	cachePath := filepath.Join(tempDir, "test_inbox.json")

	ib := NewInbox(cachePath, 100)

	// First time should succeed
	if !ib.CheckAndRecord(1001) {
		t.Fatalf("expected update 1001 to be accepted")
	}

	// Immediate duplicate check
	if !ib.IsDuplicate(1001) {
		t.Fatalf("expected update 1001 to be recognized as duplicate")
	}

	// Second record call should return false (duplicate rejected)
	if ib.CheckAndRecord(1001) {
		t.Fatalf("expected update 1001 duplicate to be rejected")
	}

	// Different update should succeed
	if !ib.CheckAndRecord(1002) {
		t.Fatalf("expected update 1002 to be accepted")
	}

	if ib.Count() != 2 {
		t.Fatalf("expected count 2, got %d", ib.Count())
	}
}

func TestInboxPersistence(t *testing.T) {
	tempDir := t.TempDir()
	cachePath := filepath.Join(tempDir, "test_inbox.json")

	ib1 := NewInbox(cachePath, 100)
	ib1.CheckAndRecord(5001)
	ib1.CheckAndRecord(5002)

	// Create new inbox reading from same file
	ib2 := NewInbox(cachePath, 100)
	if !ib2.IsDuplicate(5001) {
		t.Errorf("expected 5001 to persist across restart")
	}
	if !ib2.IsDuplicate(5002) {
		t.Errorf("expected 5002 to persist across restart")
	}
	if ib2.IsDuplicate(5003) {
		t.Errorf("did not expect 5003 to be present")
	}
}

func TestInboxPruning(t *testing.T) {
	tempDir := t.TempDir()
	cachePath := filepath.Join(tempDir, "test_inbox.json")

	// Max 3 entries
	ib := NewInbox(cachePath, 3)
	ib.CheckAndRecord(1)
	ib.CheckAndRecord(2)
	ib.CheckAndRecord(3)
	ib.CheckAndRecord(4) // should evict 1

	if ib.IsDuplicate(1) {
		t.Errorf("expected 1 to have been pruned")
	}
	if !ib.IsDuplicate(2) {
		t.Errorf("expected 2 to remain")
	}
	if !ib.IsDuplicate(4) {
		t.Errorf("expected 4 to remain")
	}
	if ib.Count() != 3 {
		t.Errorf("expected 3 entries, got %d", ib.Count())
	}
}

func TestInboxConcurrency(t *testing.T) {
	tempDir := t.TempDir()
	cachePath := filepath.Join(tempDir, "test_inbox.json")

	ib := NewInbox(cachePath, 1000)
	var wg sync.WaitGroup
	accepted := make([]bool, 50)

	// 50 concurrent attempts for the same update ID 9999
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			accepted[idx] = ib.CheckAndRecord(9999)
		}(i)
	}
	wg.Wait()

	trueCount := 0
	for _, wasAccepted := range accepted {
		if wasAccepted {
			trueCount++
		}
	}

	if trueCount != 1 {
		t.Fatalf("expected exactly 1 goroutine to succeed recording update 9999, got %d", trueCount)
	}
}
