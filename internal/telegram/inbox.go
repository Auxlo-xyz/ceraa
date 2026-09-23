package telegram

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Inbox provides thread-safe deduplication for Telegram updates.
// It prevents duplicate processing of user requests caused by network retries
// or unstable mobile connections, matching Unreal Agent's inbox guarantees.
type Inbox struct {
	mu         sync.RWMutex
	seen       map[int]time.Time
	order      []int // maintains chronological order for FIFO pruning
	filePath   string
	maxEntries int
}

type inboxRecord struct {
	UpdateID  int   `json:"update_id"`
	Timestamp int64 `json:"timestamp"`
}

type inboxData struct {
	Records []inboxRecord `json:"records"`
}

var (
	defaultInbox     *Inbox
	defaultInboxOnce sync.Once
)

// NewInbox creates an Inbox with an optional persistent JSON cache and maximum size.
func NewInbox(filePath string, maxEntries int) *Inbox {
	if maxEntries <= 0 {
		maxEntries = 5000
	}
	ib := &Inbox{
		seen:       make(map[int]time.Time),
		order:      make([]int, 0, maxEntries),
		filePath:   filePath,
		maxEntries: maxEntries,
	}
	if filePath != "" {
		_ = ib.load()
	}
	return ib
}

// GetDefaultInbox returns the singleton Inbox instance backed by sessions/inbox.json.
func GetDefaultInbox() *Inbox {
	defaultInboxOnce.Do(func() {
		dir := "sessions"
		_ = os.MkdirAll(dir, 0755)
		defaultInbox = NewInbox(filepath.Join(dir, "inbox.json"), 5000)
	})
	return defaultInbox
}

// IsDuplicate returns true if the updateID has already been seen in the cache.
func (ib *Inbox) IsDuplicate(updateID int) bool {
	if updateID <= 0 {
		return false
	}
	ib.mu.RLock()
	defer ib.mu.RUnlock()
	_, exists := ib.seen[updateID]
	return exists
}

// CheckAndRecord checks if an updateID is new and atomically records it.
// Returns true if this is a fresh update (not seen before), or false if it is a duplicate.
func (ib *Inbox) CheckAndRecord(updateID int) bool {
	if updateID <= 0 {
		return true // don't block requests with no valid update_id
	}

	ib.mu.Lock()
	defer ib.mu.Unlock()

	if _, exists := ib.seen[updateID]; exists {
		return false
	}

	ib.seen[updateID] = time.Now()
	ib.order = append(ib.order, updateID)

	// Bounded cache: prune oldest entries when exceeding maxEntries
	if len(ib.order) > ib.maxEntries {
		excess := len(ib.order) - ib.maxEntries
		for i := 0; i < excess; i++ {
			oldID := ib.order[i]
			delete(ib.seen, oldID)
		}
		ib.order = ib.order[excess:]
	}

	ib.saveLocked()
	return true
}

func (ib *Inbox) load() error {
	ib.mu.Lock()
	defer ib.mu.Unlock()

	if ib.filePath == "" {
		return nil
	}

	data, err := os.ReadFile(ib.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var d inboxData
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("decode inbox cache: %w", err)
	}

	for _, rec := range d.Records {
		if rec.UpdateID > 0 {
			ib.seen[rec.UpdateID] = time.Unix(rec.Timestamp, 0)
			ib.order = append(ib.order, rec.UpdateID)
		}
	}
	return nil
}

func (ib *Inbox) saveLocked() {
	if ib.filePath == "" {
		return
	}

	dir := filepath.Dir(ib.filePath)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}

	records := make([]inboxRecord, 0, len(ib.order))
	for _, id := range ib.order {
		records = append(records, inboxRecord{
			UpdateID:  id,
			Timestamp: ib.seen[id].Unix(),
		})
	}

	payload, err := json.MarshalIndent(inboxData{Records: records}, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(ib.filePath, payload, 0644)
}

// Count returns the number of tracked updates in the cache.
func (ib *Inbox) Count() int {
	ib.mu.RLock()
	defer ib.mu.RUnlock()
	return len(ib.seen)
}

// Clear flushes the cache and removes the persistence file.
func (ib *Inbox) Clear() {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	ib.seen = make(map[int]time.Time)
	ib.order = make([]int, 0, ib.maxEntries)
	if ib.filePath != "" {
		_ = os.Remove(ib.filePath)
	}
}
