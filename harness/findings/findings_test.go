package findings

// findings_test.go covers the record itself: what it accepts, what it
// refuses, how it orders itself and what it writes to disk.
//
// The refusal cases matter as much as the acceptance cases. A record that
// stores a status nobody defined, or an id that points out of the store
// directory, is a record that lies about what was checked.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newStore opens a store in a scratch directory.
func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "findings"))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	return store
}

func TestPutStoresAndReadsBackAFinding(t *testing.T) {
	store := newStore(t)
	stored, err := store.Put(Finding{
		Title:       "SQL injection in the login form",
		Severity:    SeverityHigh,
		Status:      StatusConfirmed,
		URL:         "https://example.com/login",
		Endpoint:    "POST /login",
		Description: "The username parameter is concatenated into a query.",
		PoC:         "curl -s 'https://example.com/login' -d \"u=' OR 1=1--\"",
		Evidence:    "Two rows came back for one username.",
		Tags:        []string{"injection", "auth"},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if stored.ID == "" {
		t.Fatal("the store did not give the finding an id")
	}
	if stored.CreatedAt.IsZero() || stored.UpdatedAt.IsZero() {
		t.Errorf("the store did not stamp the finding: %+v", stored)
	}

	got, ok := store.Get(stored.ID)
	if !ok {
		t.Fatalf("the finding was not written: %s", stored.ID)
	}
	if got.Title != stored.Title || got.PoC != stored.PoC || got.Severity != SeverityHigh {
		t.Errorf("read back %+v, want %+v", got, stored)
	}
	if len(got.Tags) != 2 {
		t.Errorf("tags = %v, want two", got.Tags)
	}
}

func TestTheRecordIsWrittenPrivate(t *testing.T) {
	store := newStore(t)
	stored, err := store.Put(Finding{Title: "A finding"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	info, err := os.Stat(filepath.Join(store.Dir(), stored.ID+".json"))
	if err != nil {
		t.Fatalf("stat the record: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the record is mode %o, want 0600", perm)
	}
	dir, err := os.Stat(store.Dir())
	if err != nil {
		t.Fatalf("stat the store: %v", err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("the store is mode %o, want 0700", perm)
	}
}

func TestPutUpsertsByIDAndKeepsTheCreationTime(t *testing.T) {
	store := newStore(t)
	first, err := store.Put(Finding{Title: "A finding", Severity: SeverityLow, Status: StatusOpenProofGap})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := store.Put(Finding{ID: first.ID, Title: "A finding", Severity: SeverityCritical, Status: StatusConfirmed})
	if err != nil {
		t.Fatalf("put again: %v", err)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at moved from %v to %v", first.CreatedAt, second.CreatedAt)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("updated_at did not move: %v then %v", first.UpdatedAt, second.UpdatedAt)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("the record holds %d findings, want 1", len(list))
	}
	if list[0].Severity != SeverityCritical {
		t.Errorf("severity = %q, want the updated one", list[0].Severity)
	}
}

// The closure vocabulary is the whole point of the record, so a status
// outside it has to be refused by name.
func TestPutRefusesAStatusOutsideTheVocabulary(t *testing.T) {
	store := newStore(t)
	_, err := store.Put(Finding{Title: "A finding", Status: Status("probably")})
	if err == nil {
		t.Fatal("a status outside the vocabulary was accepted")
	}
	for _, name := range statusNames {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %q: %v", name, err)
		}
	}
}

func TestPutRefusesASeverityOutsideTheVocabulary(t *testing.T) {
	store := newStore(t)
	if _, err := store.Put(Finding{Title: "A finding", Severity: "catastrophic"}); err == nil {
		t.Fatal("a severity outside the vocabulary was accepted")
	}
}

// An id names a file inside the store, so an id that walks out of it is a way
// out of the store rather than a name in it.
func TestPutRefusesAnIDThatEscapesTheStore(t *testing.T) {
	store := newStore(t)
	for _, id := range []string{"../escape", "sub/dir", ".hidden", "with space"} {
		if _, err := store.Put(Finding{ID: id, Title: "A finding"}); err == nil {
			t.Errorf("id %q was accepted", id)
		}
	}
	// An absent id is not an escape, it is a request for one.
	stored, err := store.Put(Finding{Title: "A finding"})
	if err != nil {
		t.Fatalf("put without an id: %v", err)
	}
	if stored.ID == "" {
		t.Error("the store did not generate an id")
	}
}

func TestListSortsWorstFirstThenMostRecent(t *testing.T) {
	store := newStore(t)
	records := []Finding{
		{Title: "Low one", Severity: SeverityLow, Status: StatusConfirmed},
		{Title: "Medium one", Severity: SeverityMedium, Status: StatusConfirmed},
		{Title: "High one", Severity: SeverityHigh, Status: StatusConfirmed},
		{Title: "Critical one", Severity: SeverityCritical, Status: StatusConfirmed},
	}
	for _, record := range records {
		if _, err := store.Put(record); err != nil {
			t.Fatalf("put %s: %v", record.Title, err)
		}
	}
	// A second finding of the same severity, written later, comes first: it
	// is the one the run was working on when it stopped.
	if _, err := store.Put(Finding{Title: "Critical two", Severity: SeverityCritical, Status: StatusConfirmed}); err != nil {
		t.Fatalf("put the second critical: %v", err)
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"Critical two", "Critical one", "High one", "Medium one", "Low one"}
	if len(list) != len(want) {
		t.Fatalf("list holds %d findings, want %d", len(list), len(want))
	}
	for i, title := range want {
		if list[i].Title != title {
			t.Errorf("finding %d is %q, want %q", i, list[i].Title, title)
		}
	}
}

// The rendered report lives in the same directory as the findings and is not
// a finding. Reading it back as one would put "report.md" in the record.
func TestListIgnoresTheRenderedReport(t *testing.T) {
	store := newStore(t)
	if _, err := store.Put(Finding{Title: "A finding"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), "report.md"), []byte("# report"), 0o600); err != nil {
		t.Fatalf("write the report: %v", err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("the record holds %d findings, want 1", len(list))
	}
}

// A record that cannot be decoded is an error, not a skip. A disclosure that
// silently drops one finding is the failure this package exists to prevent.
func TestListRefusesACorruptRecord(t *testing.T) {
	store := newStore(t)
	broken := filepath.Join(store.Dir(), "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write the broken record: %v", err)
	}
	if _, err := store.List(); err == nil {
		t.Fatal("a corrupt record was skipped instead of reported")
	}
}

func TestDeleteRemovesTheFinding(t *testing.T) {
	store := newStore(t)
	stored, err := store.Put(Finding{Title: "A finding"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Delete(stored.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := store.Get(stored.ID); ok {
		t.Error("the finding survived the delete")
	}
	if err := store.Delete(stored.ID); err == nil {
		t.Error("deleting a finding that is not there was accepted")
	}
}

func TestCoverageCountsTheRecord(t *testing.T) {
	store := newStore(t)
	records := []Finding{
		{Title: "Confirmed one", Severity: SeverityHigh, Status: StatusConfirmed},
		{Title: "Confirmed two", Severity: SeverityMedium, Status: StatusConfirmed},
		{Title: "Ruled out one", Severity: SeverityInfo, Status: StatusRuledOut},
		{Title: "Gap one", Severity: SeverityLow, Status: StatusOpenProofGap},
	}
	for _, record := range records {
		if _, err := store.Put(record); err != nil {
			t.Fatalf("put %s: %v", record.Title, err)
		}
	}
	coverage, err := store.Coverage()
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if coverage.Total != 4 {
		t.Errorf("total = %d, want 4", coverage.Total)
	}
	if coverage.ByStatus[StatusConfirmed] != 2 {
		t.Errorf("confirmed = %d, want 2", coverage.ByStatus[StatusConfirmed])
	}
	if coverage.BySeverity[SeverityHigh] != 1 {
		t.Errorf("high = %d, want 1", coverage.BySeverity[SeverityHigh])
	}
	if len(coverage.OpenGaps) != 1 || coverage.OpenGaps[0] != "Gap one" {
		t.Errorf("open gaps = %v, want the one gap", coverage.OpenGaps)
	}
	if len(coverage.RuledOut) != 1 || coverage.RuledOut[0] != "Ruled out one" {
		t.Errorf("ruled out = %v, want the one ruled out item", coverage.RuledOut)
	}
}

func TestSlugIsDeterministicSafeAndDistinct(t *testing.T) {
	first := Slug("SQL injection in /admin")
	if first != Slug("SQL injection in /admin") {
		t.Error("the same title gave two ids")
	}
	for _, r := range first {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			t.Errorf("the id carries %q, which is not safe as a file name or a URL", r)
		}
	}
	other := Slug("SQL injection in /users")
	if first == other {
		t.Errorf("two titles gave the same id %q", first)
	}
	if Slug("///") == "" {
		t.Error("a title with nothing usable in it gave an empty id")
	}
}

func TestWriteReportWritesIntoTheStore(t *testing.T) {
	store := newStore(t)
	if _, err := store.Put(Finding{Title: "A finding", Status: StatusConfirmed, Severity: SeverityHigh}); err != nil {
		t.Fatalf("put: %v", err)
	}
	path, err := store.WriteReport("md")
	if err != nil {
		t.Fatalf("write the report: %v", err)
	}
	if filepath.Dir(path) != store.Dir() {
		t.Errorf("the report landed in %q, want the store directory", filepath.Dir(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the report: %v", err)
	}
	if !strings.Contains(string(data), "A finding") {
		t.Errorf("the report does not carry the finding:\n%s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the report: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the report is mode %o, want 0600", perm)
	}
	if _, err := store.WriteReport("pdf"); err == nil {
		t.Error("an unknown report format was accepted")
	}
}
