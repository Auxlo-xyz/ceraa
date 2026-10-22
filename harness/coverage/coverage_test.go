package coverage

import (
	"path/filepath"
	"testing"
)

func TestRecordAndList(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Record(Entry{Technique: "CORS_MISCONFIG", Target: "https://example.com", Detail: "four origins"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := store.Record(Entry{Technique: "idor", Target: "https://example.com/api/user", Outcome: Finding}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	entries, err := store.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Technique != "cors_misconfig" {
		t.Errorf("the technique was not normalised: %q", entries[0].Technique)
	}
	if entries[0].Outcome != Clean {
		t.Errorf("the outcome default is %q, want clean", entries[0].Outcome)
	}

	only, err := store.List("idor", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(only) != 1 || only[0].Technique != "idor" {
		t.Errorf("the filter returned %v", only)
	}
	byTarget, err := store.List("", "example.com/api")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(byTarget) != 1 {
		t.Errorf("the target filter returned %d entries, want 1", len(byTarget))
	}
}

func TestRecordRefusesAnEmptyEntry(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Record(Entry{Target: "https://example.com"}); err == nil {
		t.Error("an entry without a technique was accepted")
	}
	if _, err := store.Record(Entry{Technique: "cors"}); err == nil {
		t.Error("an entry without a target was accepted")
	}
}

func TestGapsNameWhatWasNotTried(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Record(Entry{Technique: "cors_misconfig", Target: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	gaps, err := store.Gaps("https://example.com", nil)
	if err != nil {
		t.Fatalf("Gaps: %v", err)
	}
	for _, technique := range gaps {
		if technique == "cors_misconfig" {
			t.Error("a technique that was tried was reported as a gap")
		}
	}
	if len(gaps) != len(Catalogue())-1 {
		t.Errorf("got %d gaps, want %d", len(gaps), len(Catalogue())-1)
	}
}

func TestTheLedgerSurvivesAReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Record(Entry{Technique: "xss", Target: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := reopened.List("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the ledger did not survive a reopen: %v", entries)
	}
	if filepath.Base(reopened.path()) != "coverage.json" {
		t.Errorf("the ledger file is %s", reopened.path())
	}
}

func TestTheCatalogueIsSortedAndCoversTheReviewSurface(t *testing.T) {
	catalogue := Catalogue()
	for i := 1; i < len(catalogue); i++ {
		if catalogue[i-1] > catalogue[i] {
			t.Fatalf("the catalogue is not sorted at %d: %q then %q", i, catalogue[i-1], catalogue[i])
		}
	}
	for _, want := range []string{"auth_bypass", "idor", "cors_misconfig", "sql_injection", "xss", "ssrf", "secret_exposure", "port_scan"} {
		found := false
		for _, technique := range catalogue {
			if technique == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the catalogue is missing %q", want)
		}
	}
}
