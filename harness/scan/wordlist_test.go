package scan

// wordlist_test.go covers the wordlist loader and the embedded list. The
// loader is the boundary between a caller-supplied file and a sweep, so the
// things that matter are the rules it applies (comments, blanks, duplicates,
// the count) and the entries the embedded list actually carries.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadWordlistSkipsCommentsBlanksAndDuplicates: the rules a wordlist
// file is read under.
func TestLoadWordlistSkipsCommentsBlanksAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "words.txt")
	content := strings.Join([]string{
		"# a comment",
		"",
		"   ",
		"  admin  ",
		"admin",
		"backup.zip",
		"# another comment",
		"\tconfig.php\t",
		"backup.zip",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := LoadWordlist(path)
	if err != nil {
		t.Fatalf("LoadWordlist: %v", err)
	}
	want := []string{"admin", "backup.zip", "config.php"}
	if len(entries) != len(want) {
		t.Fatalf("entries = %v, want %v", entries, want)
	}
	for i, entry := range want {
		if entries[i] != entry {
			t.Errorf("entry %d = %q, want %q", i, entries[i], entry)
		}
	}
}

// TestLoadWordlistBoundsTheCount: a wordlist is caller-supplied input and
// an input with no bound is a way to spend an operator's budget.
func TestLoadWordlistBoundsTheCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many.txt")
	var b strings.Builder
	for i := 0; i < maxWordlistEntries+500; i++ {
		b.WriteString("word")
		b.WriteString(strings.Repeat("x", i%3))
		b.WriteString(itoa(i))
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := LoadWordlist(path)
	if err != nil {
		t.Fatalf("LoadWordlist: %v", err)
	}
	if len(entries) != maxWordlistEntries {
		t.Errorf("entries = %d, want the %d cap", len(entries), maxWordlistEntries)
	}
}

// TestLoadWordlistWithAnEmptyPathReturnsTheDefault: a caller with no list
// and a caller that asked for the default end up in the same place.
func TestLoadWordlistWithAnEmptyPathReturnsTheDefault(t *testing.T) {
	entries, err := LoadWordlist("")
	if err != nil {
		t.Fatalf("LoadWordlist: %v", err)
	}
	if len(entries) != len(DefaultWordlist()) {
		t.Errorf("the empty path returned %d entries, want the %d of the default list", len(entries), len(DefaultWordlist()))
	}
}

// TestLoadWordlistNamesAMissingFile: the path goes in the error, so a
// retry can correct it.
func TestLoadWordlistNamesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nowhere.txt")
	_, err := LoadWordlist(path)
	if err == nil {
		t.Fatal("a missing wordlist was accepted")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the error does not name the path: %v", err)
	}
}

// TestDefaultWordlistHoldsTheEntriesThatMatter: the embedded list is the
// difference between a sweep that finds something and one that does not.
func TestDefaultWordlistHoldsTheEntriesThatMatter(t *testing.T) {
	entries := DefaultWordlist()
	if len(entries) < 200 {
		t.Fatalf("the embedded list holds %d entries, want at least 200", len(entries))
	}
	have := make(map[string]bool, len(entries))
	for _, entry := range entries {
		have[entry] = true
	}
	for _, want := range []string{
		// credentials and secrets
		".env", ".git/config", ".git/HEAD", ".svn/entries", "backup.zip", "backup.sql",
		"dump.sql", "wp-config.php.bak", "id_rsa", ".aws/credentials",
		// server and framework endpoints
		"server-status", "actuator/health", "actuator/env", ".DS_Store", "composer.json",
		"package.json", "swagger.json", "openapi.json", "graphql", "wp-admin", "phpmyadmin",
		"adminer.php", "debug", "trace", "healthz", "metrics", "sitemap.xml", "robots.txt",
		"crossdomain.xml", ".well-known/security.txt", "server-info", "phpinfo.php",
		"elmah.axd", "web.config", "config.php", "config.json", "config.yml", "settings.py",
		"credentials", "secrets", "token", "oauth", "callback", "webhook", "webhooks",
		"cgi-bin", ".well-known", "vendor", "node_modules", "webpack.config.js",
		"next.config.js", "env.js", "config.js",
		// the obvious admin and identity paths
		"admin", "api", "login", "auth", "user", "users", "accounts", "panel", "dashboard",
		"console",
	} {
		if !have[want] {
			t.Errorf("the embedded list is missing %q", want)
		}
	}
}

// TestDefaultWordlistHandsOutACopy: a caller that sorts the list must not
// change what the next caller gets.
func TestDefaultWordlistHandsOutACopy(t *testing.T) {
	first := DefaultWordlist()
	if len(first) < 2 {
		t.Fatalf("the embedded list holds %d entries", len(first))
	}
	first[0] = "mutated"
	second := DefaultWordlist()
	if second[0] == "mutated" {
		t.Error("the embedded list was changed through the returned slice")
	}
}

// itoa is a tiny helper so the wordlist builder does not pull strconv into
// the test for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
