package compaction

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFoldRetainReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	folds := filepath.Join(dir, "folds")
	msgs := []FoldMessage{
		{Role: "user", Text: "hello world, this is a test message"},
		{Role: "assistant", Text: "acknowledged, here is a longer reply with more content"},
	}
	path, err := RetainFold(folds, "block-abc_123", msgs)
	if err != nil {
		t.Fatalf("RetainFold: %v", err)
	}
	want := filepath.Join(folds, "fold-block-abc_123.json")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat retained file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file perm = %o, want 600", perm)
	}
	if perm := dirPerm(t, folds); perm != 0o700 {
		t.Fatalf("dir perm = %o, want 700", perm)
	}
	rec, err := ReadFold(path)
	if err != nil {
		t.Fatalf("ReadFold: %v", err)
	}
	if rec.BlockID != "block-abc_123" {
		t.Fatalf("BlockID = %q, want %q", rec.BlockID, "block-abc_123")
	}
	if rec.MessageCount != len(msgs) {
		t.Fatalf("MessageCount = %d, want %d", rec.MessageCount, len(msgs))
	}
	wantTokens := EstimateTokens(msgs[0].Text) + EstimateTokens(msgs[1].Text)
	if rec.ApproxTokens != wantTokens {
		t.Fatalf("ApproxTokens = %d, want %d", rec.ApproxTokens, wantTokens)
	}
	if len(rec.Sources) != len(msgs) {
		t.Fatalf("Sources len = %d, want %d", len(rec.Sources), len(msgs))
	}
	for i := range msgs {
		if rec.Sources[i] != msgs[i] {
			t.Fatalf("Sources[%d] = %+v, want %+v", i, rec.Sources[i], msgs[i])
		}
	}
	if rec.CreatedAt.IsZero() {
		t.Fatal("CreatedAt is zero")
	}
}

func TestFoldBlockIDSanitization(t *testing.T) {
	dir := t.TempDir()
	path, err := RetainFold(dir, "../evil", []FoldMessage{{Role: "user", Text: "x"}})
	if err != nil {
		t.Fatalf("RetainFold: %v", err)
	}
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, filepath.Clean(dir)+string(filepath.Separator)) {
		t.Fatalf("path %q escapes dir %q", path, dir)
	}
	if strings.Contains(filepath.Base(path), "/") || filepath.Base(path) == ".." {
		t.Fatalf("unsafe basename %q", path)
	}
	if base := filepath.Base(path); base != "fold-___evil.json" {
		t.Fatalf("basename = %q, want %q", base, "fold-___evil.json")
	}
	rec, err := ReadFold(path)
	if err != nil {
		t.Fatalf("ReadFold: %v", err)
	}
	if rec.BlockID != "../evil" {
		t.Fatalf("BlockID = %q, want original %q", rec.BlockID, "../evil")
	}
}

func TestFoldReadMissingFile(t *testing.T) {
	_, err := ReadFold(filepath.Join(t.TempDir(), "fold-nope.json"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestFoldEstimateTokens(t *testing.T) {
	if got := EstimateTokens("abcd"); got != 1 {
		t.Fatalf("EstimateTokens(4 bytes) = %d, want 1", got)
	}
	if got := EstimateTokens(""); got != 0 {
		t.Fatalf("EstimateTokens(empty) = %d, want 0", got)
	}
}

func dirPerm(t *testing.T, dir string) os.FileMode {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	return info.Mode().Perm()
}
