// Package compaction persists compacted source text so folds stay restorable.
//
// RetainFold writes one plain JSON file per fold; ReadFold reads it back.
// Errors are descriptive and never fatal to callers, who log and continue.
package compaction

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FoldMessage is one compacted source message kept for fold restoration.
type FoldMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// FoldRecord is the retained source text behind one folded block.
type FoldRecord struct {
	BlockID      string        `json:"block_id"`
	CreatedAt    time.Time     `json:"created_at"`
	MessageCount int           `json:"message_count"`
	ApproxTokens int           `json:"approx_tokens"`
	Sources      []FoldMessage `json:"sources"`
}

// Note: token estimation reuses the existing EstimateTokens in compaction.go
// (char/4, same as soraacode token.ts).

// sanitizeBlockID maps every byte outside [A-Za-z0-9_-] to underscore so the
// block ID can never escape dir as a path separator or traversal.
func sanitizeBlockID(blockID string) string {
	var b strings.Builder
	b.Grow(len(blockID))
	for i := 0; i < len(blockID); i++ {
		c := blockID[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// RetainFold writes messages as one JSON file under dir named
// fold-<blockID>.json, creating dir with 0700 and the file with 0600.
// It returns the file path.
func RetainFold(dir, blockID string, messages []FoldMessage) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("compaction: retain fold: empty dir")
	}
	if strings.TrimSpace(blockID) == "" {
		return "", fmt.Errorf("compaction: retain fold: empty block ID")
	}
	safe := sanitizeBlockID(blockID)
	if strings.Trim(safe, "_") == "" {
		return "", fmt.Errorf("compaction: retain fold: block ID %q has no usable characters", blockID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("compaction: retain fold: create dir %q: %w", dir, err)
	}
	sources := make([]FoldMessage, len(messages))
	copy(sources, messages)
	tokens := 0
	for _, m := range sources {
		tokens += EstimateTokens(m.Text)
	}
	rec := FoldRecord{
		BlockID:      blockID,
		CreatedAt:    time.Now().UTC(),
		MessageCount: len(sources),
		ApproxTokens: tokens,
		Sources:      sources,
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("compaction: retain fold: encode block %q: %w", blockID, err)
	}
	if len(data) > ArchiveMaxBytes {
		return "", fmt.Errorf("compaction: retain fold: block %q is %d bytes, exceeds %d-byte limit", blockID, len(data), ArchiveMaxBytes)
	}
	path := filepath.Join(dir, "fold-"+safe+".json")
	if err := writeFoldAtomic(path, data); err != nil {
		return "", fmt.Errorf("compaction: retain fold: write %q: %w", path, err)
	}
	if err := pruneFolds(dir, ArchiveKeep); err != nil {
		return "", fmt.Errorf("compaction: retain fold: prune %q: %w", dir, err)
	}
	return path, nil
}

func writeFoldAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".fold-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return nil
}

func pruneFolds(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type foldFile struct {
		name string
		at   time.Time
	}
	files := make([]foldFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "fold-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files = append(files, foldFile{name: entry.Name(), at: info.ModTime()})
	}
	if keep < 0 {
		keep = 0
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	for len(files) > keep {
		if err := os.Remove(filepath.Join(dir, files[0].name)); err != nil && !os.IsNotExist(err) {
			return err
		}
		files = files[1:]
	}
	return nil
}

func ReadFold(path string) (FoldRecord, error) {
	var rec FoldRecord
	if strings.TrimSpace(path) == "" {
		return rec, fmt.Errorf("compaction: read fold: empty path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return rec, fmt.Errorf("compaction: read fold: read %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("compaction: read fold: decode %q: %w", path, err)
	}
	return rec, nil
}
