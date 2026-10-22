package history

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeReceipt writes one receipt the way the probe does.
func writeReceipt(t *testing.T, dir string, name string, at time.Time, method string, url string, status int, body string) {
	t.Helper()
	doc := map[string]any{
		"at":        at.Format(time.RFC3339Nano),
		"url":       url,
		"method":    method,
		"status":    status,
		"sha256":    "abcdef0123456789",
		"headers":   map[string]string{"content-type": "text/html"},
		"body_head": body,
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestQueryReadsTheReceipts(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeReceipt(t, dir, "a", now.Add(-2*time.Minute), "GET", "https://example.com/admin", 200, "shell")
	writeReceipt(t, dir, "b", now.Add(-time.Minute), "POST", "https://example.com/login", 302, "redirect")
	writeReceipt(t, dir, "c", now, "GET", "https://other.test/x", 404, "missing")

	entries, err := Query(dir, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	if entries[0].ID != "c" {
		t.Errorf("the entries are not newest first: %v", entries)
	}

	byHost, err := Query(dir, Filter{Host: "example.com"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(byHost) != 2 {
		t.Errorf("the host filter returned %d entries, want 2", len(byHost))
	}

	byMethod, err := Query(dir, Filter{Method: "POST"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(byMethod) != 1 || byMethod[0].URL != "https://example.com/login" {
		t.Errorf("the method filter returned %v", byMethod)
	}

	byPath, err := Query(dir, Filter{PathContains: "/admin"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(byPath) != 1 {
		t.Errorf("the path filter returned %d entries, want 1", len(byPath))
	}

	limited, err := Query(dir, Filter{Limit: 2})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("the limit returned %d entries, want 2", len(limited))
	}
}

func TestQueryOnAnEmptyDirectory(t *testing.T) {
	entries, err := Query(t.TempDir(), Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("an empty trail returned %d entries", len(entries))
	}
}

func TestHostsListsWhatWasContacted(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeReceipt(t, dir, "a", now, "GET", "https://example.com/one", 200, "")
	writeReceipt(t, dir, "b", now, "GET", "https://example.com/two", 200, "")
	writeReceipt(t, dir, "c", now, "GET", "https://other.test/x", 200, "")
	hosts, err := Hosts(dir)
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if len(hosts) != 2 || hosts[0] != "example.com" {
		t.Errorf("hosts = %v", hosts)
	}
}

func TestApplyQueryReplacesTheValue(t *testing.T) {
	updated, err := applyQuery("https://example.com/a?x=1&y=2", map[string]string{"x": "9"})
	if err != nil {
		t.Fatalf("applyQuery: %v", err)
	}
	if updated != "https://example.com/a?x=9&y=2" {
		t.Errorf("the query was not replaced: %s", updated)
	}
}

func TestReplayRefusesAnOutOfScopeTarget(t *testing.T) {
	// The replay goes through the same gate as any other request, so a
	// target the operator never authorized is refused before it is dialed.
	_, err := Replay(context.Background(), Entry{URL: "http://127.0.0.1:1/"}, Change{})
	if err == nil {
		t.Error("a replay to a self target was allowed")
	}
}
