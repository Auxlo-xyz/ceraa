package tool

// recon_finding_test.go covers the tool face: the argument shapes it accepts,
// the refusals it makes, and what the chat sees.
//
// The store is exercised through the tool rather than around it, because the
// tool is where the model's arguments turn into a record, and a record that
// only the direct API can produce is a record the model cannot write.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/findings"
)

// findingHome points the data directory at a scratch directory and returns
// the path of the finding store inside it.
func findingHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CERAA_HOME", home)
	return filepath.Join(home, "findings")
}

// runFinding translates and runs the tool, and fails the test on an error.
func runFinding(t *testing.T, args map[string]any) string {
	t.Helper()
	tl := &FindingTool{}
	spec, err := tl.Translate(args)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	out, err := tl.Run(spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return out
}

// findingFails translates and runs the tool, and fails the test when it
// succeeds.
func findingFails(t *testing.T, args map[string]any) error {
	t.Helper()
	tl := &FindingTool{}
	spec, err := tl.Translate(args)
	if err != nil {
		return err
	}
	if _, err := tl.Run(spec); err != nil {
		return err
	}
	t.Fatalf("the tool accepted %v", args)
	return nil
}

func TestTheFindingToolIsNamedForItsOperationType(t *testing.T) {
	tl := &FindingTool{}
	if tl.Name() != "finding" {
		t.Errorf("Name = %q, want finding", tl.Name())
	}
	spec, err := tl.Translate(map[string]any{"title": "A finding"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Type != tl.Name() {
		t.Errorf("spec.Type = %q, want %q: the executor resolves the tool by the operation type", spec.Type, tl.Name())
	}
}

// The tool has to be reachable twice: offered to the model through the
// registry and executed by the operation manager. This pins the shape the
// registry needs.
func TestTheFindingToolHasTheShapeTheRegistryNeeds(t *testing.T) {
	var registered Tool = &FindingTool{}
	if registered.Schema()["type"] != "object" {
		t.Errorf("the schema is not an object: %v", registered.Schema())
	}
	properties, _ := registered.Schema()["properties"].(map[string]any)
	for _, name := range []string{"mode", "id", "title", "severity", "status", "url", "endpoint", "description", "poc", "evidence", "counterevidence", "remediation", "cvss", "tags"} {
		if _, present := properties[name]; !present {
			t.Errorf("the schema does not declare %q", name)
		}
	}
}

func TestTheFindingToolTranslatesEveryMode(t *testing.T) {
	tl := &FindingTool{}
	for _, mode := range []string{"add", "update", "list", "delete", "coverage"} {
		args := map[string]any{"mode": mode}
		if mode == "add" || mode == "update" {
			args["title"] = "A finding"
		}
		if mode == "delete" {
			args["id"] = "a-finding-a1b2c3d4"
		}
		spec, err := tl.Translate(args)
		if err != nil {
			t.Errorf("translate %s: %v", mode, err)
			continue
		}
		if spec.Type != "finding" {
			t.Errorf("mode %s gave the operation type %q", mode, spec.Type)
		}
		if spec.Args["mode"] != mode {
			t.Errorf("mode %s was carried as %v", mode, spec.Args["mode"])
		}
	}
}

func TestTheFindingToolRefusesAnUnknownMode(t *testing.T) {
	tl := &FindingTool{}
	if _, err := tl.Translate(map[string]any{"mode": "publish"}); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
}

// The closure vocabulary is the whole point of the record, so a status
// outside it is refused by name.
func TestTheFindingToolRefusesAStatusOutsideTheVocabulary(t *testing.T) {
	tl := &FindingTool{}
	_, err := tl.Translate(map[string]any{"title": "A finding", "status": "probably"})
	if err == nil {
		t.Fatal("a status outside the vocabulary was accepted")
	}
	for _, name := range []string{"confirmed", "ruled_out", "open_proof_gap"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %q: %v", name, err)
		}
	}
}

func TestTheFindingToolRefusesAnUpdateWithoutATitle(t *testing.T) {
	tl := &FindingTool{}
	if _, err := tl.Translate(map[string]any{"mode": "update"}); err == nil {
		t.Fatal("an update without a title was accepted")
	}
	if _, err := tl.Translate(map[string]any{"mode": "delete"}); err == nil {
		t.Fatal("a delete without an id was accepted")
	}
}

// The defaults belong to a new finding. An update that did not name a
// severity keeps the one the record carries, which is the difference between
// an update and a rewrite.
func TestTheDefaultsApplyToANewFindingOnly(t *testing.T) {
	tl := &FindingTool{}
	add, err := tl.Translate(map[string]any{"title": "A finding"})
	if err != nil {
		t.Fatalf("translate add: %v", err)
	}
	if add.Args["severity"] != findings.SeverityMedium {
		t.Errorf("a new finding got the severity %v, want %v", add.Args["severity"], findings.SeverityMedium)
	}
	if add.Args["status"] != findings.StatusConfirmed {
		t.Errorf("a new finding got the status %v, want %v", add.Args["status"], findings.StatusConfirmed)
	}
	update, err := tl.Translate(map[string]any{"mode": "update", "title": "A finding"})
	if err != nil {
		t.Fatalf("translate update: %v", err)
	}
	if _, present := update.Args["severity"]; present {
		t.Errorf("the update defaulted the severity to %v", update.Args["severity"])
	}
	if _, present := update.Args["status"]; present {
		t.Errorf("the update defaulted the status to %v", update.Args["status"])
	}
}

func TestTheFindingToolComputesTheCVSSScore(t *testing.T) {
	tl := &FindingTool{}
	spec, err := tl.Translate(map[string]any{
		"title": "A finding",
		"cvss":  "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Args["cvss_score"] != 9.8 {
		t.Errorf("the score is %v, want 9.8", spec.Args["cvss_score"])
	}
	if spec.Args["cvss_vector"] != "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H" {
		t.Errorf("the vector is %v", spec.Args["cvss_vector"])
	}
	// The eight metrics on their own are the score.
	spec, err = tl.Translate(map[string]any{"title": "A finding", "cvss": "AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"})
	if err != nil {
		t.Fatalf("translate the bare metrics: %v", err)
	}
	if spec.Args["cvss_score"] != 9.8 {
		t.Errorf("the score is %v, want 9.8", spec.Args["cvss_score"])
	}
	if _, err := tl.Translate(map[string]any{"title": "A finding", "cvss": "CVSS:3.1/AV:Z/AC:L"}); err == nil {
		t.Error("a vector nobody can score was accepted")
	}
}

func TestTheFindingToolRecordsAFinding(t *testing.T) {
	dir := findingHome(t)
	out := runFinding(t, map[string]any{
		"title":    "SQL injection in the login form",
		"severity": "high",
		"status":   "confirmed",
		"url":      "https://example.com/login",
		"poc":      "curl -s 'https://example.com/login' -d \"u=' OR 1=1--\"",
	})
	if !strings.Contains(out, "recorded finding") {
		t.Errorf("the tool did not confirm the record:\n%s", out)
	}
	// The reproduction is the thing a reader copies, so it is printed as it
	// was given.
	if !strings.Contains(out, "curl -s 'https://example.com/login' -d \"u=' OR 1=1--\"") {
		t.Errorf("the reproduction was not printed verbatim:\n%s", out)
	}
	if !strings.Contains(out, "severity: high") || !strings.Contains(out, "status: confirmed") {
		t.Errorf("the block does not carry the severity and the state:\n%s", out)
	}

	store, err := findings.Open(dir)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("the record holds %d findings, want 1", len(list))
	}
	if list[0].Title != "SQL injection in the login form" || list[0].Status != findings.StatusConfirmed {
		t.Errorf("the record holds %+v", list[0])
	}
}

func TestTheFindingToolUpdatesOnlyTheFieldsItNames(t *testing.T) {
	dir := findingHome(t)
	runFinding(t, map[string]any{
		"title":    "SQL injection in the login form",
		"severity": "high",
		"status":   "open_proof_gap",
		"evidence": "One row came back for one username.",
	})
	store, err := findings.Open(dir)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	id := list[0].ID

	out := runFinding(t, map[string]any{
		"mode":  "update",
		"id":    id,
		"title": "SQL injection in the login form",
		"poc":   "curl -s 'https://example.com/login'",
	})
	if !strings.Contains(out, "updated finding "+id) {
		t.Errorf("the tool did not confirm the update:\n%s", out)
	}

	list, err = store.List()
	if err != nil {
		t.Fatalf("list again: %v", err)
	}
	updated := list[0]
	if updated.PoC != "curl -s 'https://example.com/login'" {
		t.Errorf("the reproduction was not written: %+v", updated)
	}
	if updated.Severity != findings.SeverityHigh {
		t.Errorf("the severity was overwritten by %q", updated.Severity)
	}
	if updated.Status != findings.StatusOpenProofGap {
		t.Errorf("the state was overwritten by %q", updated.Status)
	}
	if updated.Evidence != "One row came back for one username." {
		t.Errorf("the evidence was lost: %+v", updated)
	}
}

func TestTheFindingToolRefusesAnUpdateOfAnUnknownFinding(t *testing.T) {
	findingHome(t)
	err := findingFails(t, map[string]any{"mode": "update", "id": "nothing-here-a1b2c3d4", "title": "A finding"})
	if !strings.Contains(err.Error(), "nothing-here-a1b2c3d4") {
		t.Errorf("the error does not name the id: %v", err)
	}
}

// An id the store could never hold is refused by name, not reported as a
// finding that does not exist.
func TestTheFindingToolRefusesAnIDItCouldNeverHold(t *testing.T) {
	findingHome(t)
	err := findingFails(t, map[string]any{"mode": "update", "id": "../escape", "title": "A finding"})
	if err == nil || !strings.Contains(err.Error(), "../escape") {
		t.Errorf("the error does not name the id: %v", err)
	}
}

func TestTheFindingToolListsOneLinePerFinding(t *testing.T) {
	findingHome(t)
	runFinding(t, map[string]any{"title": "First finding", "severity": "high", "url": "https://example.com/a"})
	runFinding(t, map[string]any{"title": "Second finding", "severity": "low", "url": "https://example.com/b"})
	out := runFinding(t, map[string]any{"mode": "list"})
	if !strings.Contains(out, "findings: 2") {
		t.Errorf("the list did not count the record:\n%s", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("the list is %d lines, want a count and two findings:\n%s", len(lines), out)
	}
	for _, want := range []string{"First finding", "https://example.com/a", "high", "confirmed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the list is missing %q:\n%s", want, out)
		}
	}
}

func TestTheFindingToolDeletesAFinding(t *testing.T) {
	dir := findingHome(t)
	runFinding(t, map[string]any{"title": "A finding"})
	store, err := findings.Open(dir)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	id := list[0].ID

	out := runFinding(t, map[string]any{"mode": "delete", "id": id})
	if !strings.Contains(out, "deleted finding "+id) {
		t.Errorf("the tool did not confirm the delete:\n%s", out)
	}
	list, err = store.List()
	if err != nil {
		t.Fatalf("list after the delete: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("the record still holds %d findings", len(list))
	}
	if err := findingFails(t, map[string]any{"mode": "delete", "id": id}); err == nil {
		t.Error("deleting a finding twice was accepted")
	}
}

func TestTheFindingToolReportsTheCoverage(t *testing.T) {
	findingHome(t)
	runFinding(t, map[string]any{"title": "Confirmed one", "status": "confirmed"})
	runFinding(t, map[string]any{"title": "Ruled out one", "status": "ruled_out", "severity": "info"})
	runFinding(t, map[string]any{"title": "Gap one", "status": "open_proof_gap", "severity": "low"})
	out := runFinding(t, map[string]any{"mode": "coverage"})
	for _, want := range []string{"findings: 3", "confirmed 1", "ruled_out 1", "open_proof_gap 1", "open proof gaps:", "Gap one"} {
		if !strings.Contains(out, want) {
			t.Errorf("the coverage is missing %q:\n%s", want, out)
		}
	}
}

// The store lives under the data directory, so a run and a report read the
// same record.
func TestTheFindingStoreLivesUnderTheDataDirectory(t *testing.T) {
	dir := findingHome(t)
	runFinding(t, map[string]any{"title": "A finding"})
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the store is not at %q: %v", dir, err)
	}
}
