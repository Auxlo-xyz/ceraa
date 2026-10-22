package tool

// recon_ledger_test.go covers the tool face of the merged ledger: the
// argument shapes it accepts, the refusals it makes, and what the chat sees.
//
// The stores are exercised through the tool rather than around it, because the
// tool is where the model's arguments turn into a record, and a record only
// the direct API can produce is a record the model cannot write.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/operation"
)

// ledgerHome points the data directory at a scratch directory and returns it,
// so a test writes into a ledger and a trail of its own.
func ledgerHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CERAA_HOME", home)
	return home
}

// runLedger translates and runs the tool, and fails the test on an error.
func runLedger(t *testing.T, args map[string]any) string {
	t.Helper()
	tl := &LedgerTool{}
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

// ledgerFails translates and runs the tool, and fails the test when it
// succeeds.
func ledgerFails(t *testing.T, args map[string]any) error {
	t.Helper()
	tl := &LedgerTool{}
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

// writeReceipt puts a receipt in the trail, in the shape the probe writes.
func writeReceipt(t *testing.T, home, id, rawurl, method string, status int) {
	t.Helper()
	dir := filepath.Join(home, "recon", "receipts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("receipts dir: %v", err)
	}
	doc := fmt.Sprintf(`{"at":"2026-01-02T03:04:05Z","url":%q,"method":%q,"status":%d,"sha256":"0123456789abcdef"}`,
		rawurl, method, status)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(doc), 0o600); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
}

// TestLedgerNameIsTheOperationType: the runner resolves the executor by the
// operation type, so the two have to be the same name.
func TestLedgerNameIsTheOperationType(t *testing.T) {
	tl := &LedgerTool{}
	if tl.Name() != "ledger" {
		t.Errorf("Name = %q, want ledger", tl.Name())
	}
	spec, err := tl.Translate(map[string]any{"technique": "cors", "target": "https://example.com"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Type != tl.Name() {
		t.Errorf("spec.Type = %q, want %q", spec.Type, tl.Name())
	}
}

// TestLedgerTranslateDefaultsToRecord: a call that names a technique without
// saying what to do with it is recording one.
func TestLedgerTranslateDefaultsToRecord(t *testing.T) {
	tl := &LedgerTool{}
	spec, err := tl.Translate(map[string]any{"technique": "cors", "target": "https://example.com"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Args["mode"] != ledgerModeRecord {
		t.Errorf("mode = %v, want record", spec.Args["mode"])
	}
}

// TestLedgerTranslateRefusesAnUnknownMode: the mode chooses the store, so an
// unknown one has to be refused rather than guessed.
func TestLedgerTranslateRefusesAnUnknownMode(t *testing.T) {
	tl := &LedgerTool{}
	for _, mode := range []string{"burn", "triage", "entries"} {
		spec, err := tl.Translate(map[string]any{"mode": mode, "technique": "cors", "target": "https://example.com"})
		if err == nil {
			t.Errorf("mode %q was accepted, spec = %v", mode, spec)
			continue
		}
		if !strings.Contains(err.Error(), "mode") {
			t.Errorf("the error does not name the mode: %v", err)
		}
	}
	// A mode that arrived with the padding a model puts around it still names
	// the mode it says.
	padded, err := tl.Translate(map[string]any{"mode": " Record ", "technique": "cors", "target": "https://example.com"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if padded.Args["mode"] != ledgerModeRecord {
		t.Errorf("mode = %v, want record", padded.Args["mode"])
	}
}

// TestLedgerRecordNeedsATechniqueAndATarget: a schema cannot say that a
// field is required in one mode only, so the tool says it and names the mode.
func TestLedgerRecordNeedsATechniqueAndATarget(t *testing.T) {
	tl := &LedgerTool{}
	for _, args := range []map[string]any{
		{"mode": "record"},
		{"mode": "record", "technique": "cors"},
		{"mode": "record", "target": "https://example.com"},
		{"mode": "record", "technique": "", "target": "https://example.com"},
		{"mode": "record", "technique": "cors", "target": ""},
	} {
		spec, err := tl.Translate(args)
		if err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
			continue
		}
		if !strings.Contains(err.Error(), "mode record") {
			t.Errorf("the error does not name the mode: %v", err)
		}
	}
}

// TestLedgerReplayNeedsTheIDOfARecordedRequest: a replay without a receipt is
// a request with nothing to change.
func TestLedgerReplayNeedsTheIDOfARecordedRequest(t *testing.T) {
	tl := &LedgerTool{}
	for _, args := range []map[string]any{
		{"mode": "replay"},
		{"mode": "replay", "id": ""},
		{"mode": "replay", "new_method": "POST"},
	} {
		spec, err := tl.Translate(args)
		if err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
			continue
		}
		if !strings.Contains(err.Error(), "id") {
			t.Errorf("the error does not name what was missing: %v", err)
		}
	}
}

// TestLedgerModesKeepTheirOwnArguments: the modes are two tools wearing one
// face, so an argument of the other mode must not travel into the spec.
func TestLedgerModesKeepTheirOwnArguments(t *testing.T) {
	tl := &LedgerTool{}

	record, err := tl.Translate(map[string]any{
		"mode":          "record",
		"technique":     "cors",
		"target":        "https://example.com",
		"detail":        "origin header",
		"outcome":       "clean",
		"note":          "nothing",
		"id":            "r1",
		"host":          "example.com",
		"path_contains": "/admin",
		"method":        "GET",
		"status":        float64(200),
		"limit":         float64(10),
		"new_method":    "POST",
		"new_url":       "https://example.com/admin",
		"body":          "id=1",
		"headers":       map[string]any{"X-Test": "1"},
		"query":         map[string]any{"debug": "1"},
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	for _, name := range []string{"id", "host", "path_contains", "method", "status", "limit",
		"new_method", "new_url", "body", "headers", "remove_headers", "query"} {
		if _, ok := record.Args[name]; ok {
			t.Errorf("the record spec carries the trail argument %q: %v", name, record.Args)
		}
	}

	replay, err := tl.Translate(map[string]any{
		"mode":          "replay",
		"id":            "r1",
		"technique":     "cors",
		"target":        "https://example.com",
		"detail":        "origin header",
		"outcome":       "clean",
		"note":          "nothing",
		"host":          "example.com",
		"path_contains": "/admin",
		"method":        "GET",
		"status":        float64(200),
		"limit":         float64(10),
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	for _, name := range []string{"technique", "target", "detail", "outcome", "note",
		"host", "path_contains", "method", "status", "limit"} {
		if _, ok := replay.Args[name]; ok {
			t.Errorf("the replay spec carries the coverage argument %q: %v", name, replay.Args)
		}
	}
}

// TestLedgerTranslateReadsEveryArgument: the canonical argument map carries
// what the caller asked for, in the mode that asked for it.
func TestLedgerTranslateReadsEveryArgument(t *testing.T) {
	tl := &LedgerTool{}

	record, err := tl.Translate(map[string]any{
		"mode":      "record",
		"technique": "cors",
		"target":    "https://example.com",
		"detail":    "origin header",
		"outcome":   "clean",
		"note":      "nothing found",
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if record.Args["technique"] != "cors" || record.Args["target"] != "https://example.com" {
		t.Errorf("spec.Args = %v", record.Args)
	}
	if record.Args["detail"] != "origin header" || record.Args["outcome"] != "clean" || record.Args["note"] != "nothing found" {
		t.Errorf("spec.Args = %v", record.Args)
	}

	list, err := tl.Translate(map[string]any{
		"mode":          "list",
		"technique":     "cors",
		"target":        "https://example.com",
		"host":          "example.com",
		"path_contains": "/admin",
		"method":        "GET",
		"status":        float64(200),
		"limit":         float64(10),
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if list.Args["host"] != "example.com" || list.Args["path_contains"] != "/admin" || list.Args["method"] != "GET" {
		t.Errorf("spec.Args = %v", list.Args)
	}
	if status, _ := list.Args["status"].(int); status != 200 {
		t.Errorf("spec.Args[status] = %v (%T), want 200", list.Args["status"], list.Args["status"])
	}
	if limit, _ := list.Args["limit"].(int); limit != 10 {
		t.Errorf("spec.Args[limit] = %v (%T), want 10", list.Args["limit"], list.Args["limit"])
	}

	replay, err := tl.Translate(map[string]any{
		"mode":           "replay",
		"id":             "r1",
		"new_method":     "POST",
		"new_url":        "https://example.com/admin",
		"body":           "id=1",
		"headers":        map[string]any{"X-Forwarded-For": "127.0.0.1"},
		"remove_headers": []any{"Cookie", "Authorization"},
		"query":          map[string]any{"debug": "1"},
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if replay.Args["id"] != "r1" || replay.Args["new_method"] != "POST" || replay.Args["new_url"] != "https://example.com/admin" {
		t.Errorf("spec.Args = %v", replay.Args)
	}
	if replay.Args["body"] != "id=1" {
		t.Errorf("spec.Args[body] = %v", replay.Args["body"])
	}
	if headers, _ := replay.Args["headers"].(map[string]any); headers["X-Forwarded-For"] != "127.0.0.1" {
		t.Errorf("spec.Args[headers] = %v", replay.Args["headers"])
	}
	if removed, _ := replay.Args["remove_headers"].([]string); len(removed) != 2 || removed[0] != "Cookie" {
		t.Errorf("spec.Args[remove_headers] = %v", replay.Args["remove_headers"])
	}
	if query, _ := replay.Args["query"].(map[string]any); query["debug"] != "1" {
		t.Errorf("spec.Args[query] = %v", replay.Args["query"])
	}
}

// TestLedgerSchemaCoversEveryArgument: a property the tool reads but the
// schema does not declare is refused by the harness before the tool runs.
func TestLedgerSchemaCoversEveryArgument(t *testing.T) {
	tl := &LedgerTool{}
	schema := tl.Schema()
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{
		"mode",
		// the coverage arguments
		"technique", "target", "detail", "outcome", "note",
		// the receipt trail arguments
		"id", "host", "path_contains", "method", "status", "limit",
		"new_method", "new_url", "body", "headers", "remove_headers", "query",
	} {
		if _, ok := properties[name]; !ok {
			t.Errorf("the schema does not declare %q", name)
		}
	}
	// The mode decides what the call must carry, so nothing can be required
	// by the schema.
	required, _ := schema["required"].([]string)
	if len(required) != 0 {
		t.Errorf("required = %v, want none: the mode decides", required)
	}
	mode, _ := properties["mode"].(map[string]any)
	enum, _ := mode["enum"].([]string)
	want := []string{"record", "list", "gaps", "catalogue", "replay", "hosts"}
	if len(enum) != len(want) {
		t.Fatalf("enum = %v, want %v", enum, want)
	}
	for i, name := range want {
		if enum[i] != name {
			t.Errorf("enum = %v, want %v", enum, want)
			break
		}
	}
	// The harness validates the model's arguments against this schema before
	// the tool ever sees them.
	for _, args := range []map[string]any{
		{"mode": "record", "technique": "cors", "target": "https://example.com", "outcome": "clean"},
		{"mode": "list", "technique": "cors", "target": "https://example.com", "host": "example.com", "status": 200},
		{"mode": "gaps", "target": "https://example.com"},
		{"mode": "catalogue"},
		{"mode": "replay", "id": "r1", "new_method": "POST", "headers": map[string]any{"X": "1"}, "remove_headers": []any{"Cookie"}},
		{"mode": "hosts"},
	} {
		if err := ValidateArguments(schema, args); err != nil {
			t.Errorf("the harness refuses the declared arguments: %v", err)
		}
	}
	// An argument the tool does not read is still refused: the merged schema
	// is wider than either mode, and a wider schema is not a looser one.
	if err := ValidateArguments(schema, map[string]any{"mode": "record", "probe": "everything"}); err == nil {
		t.Error("the harness accepted an argument the tool does not read")
	}
}

// TestTheRegistryCarriesOneLedger: the point of the merge is one catalogue
// entry for one capability, because the catalogue is the cached prefix of
// every request.
func TestTheRegistryCarriesOneLedger(t *testing.T) {
	reg := DefaultRegistry()
	if _, ok := reg.Get("ledger"); !ok {
		t.Fatal("ledger is not registered")
	}
	offered := 0
	for _, tl := range reg.Offered() {
		switch tl.Name() {
		case "ledger":
			offered++
		case "coverage", "history":
			t.Errorf("%q is still offered to the model, so the merge left two entries for one capability", tl.Name())
		}
	}
	if offered != 1 {
		t.Errorf("the catalogue carries %d ledger entries, want 1", offered)
	}
}

// TestLedgerDescriptionNamesTheContract: the model reads this, so it has to
// say what the tool is and what it holds.
func TestLedgerDescriptionNamesTheContract(t *testing.T) {
	description := (&LedgerTool{}).Description()
	for _, want := range []string{"coverage", "receipt", "replay"} {
		if !strings.Contains(description, want) {
			t.Errorf("the description does not mention %q: %s", want, description)
		}
	}
	// A description is the tool's contract, not the prompt's guidance.
	for _, phrase := range []string{"use this", "use it to", "rather than", "prefer this"} {
		if strings.Contains(strings.ToLower(description), phrase) {
			t.Errorf("the description carries usage guidance (%q): %s", phrase, description)
		}
	}
}

// TestLedgerRecordsAndReadsBackTheCoverageLedger: an attempt that was
// recorded has to come back, with the detail and the note it was given.
func TestLedgerRecordsAndReadsBackTheCoverageLedger(t *testing.T) {
	ledgerHome(t)
	out := runLedger(t, map[string]any{
		"mode":      "record",
		"technique": "cors",
		"target":    "https://example.com",
		"detail":    "origin header",
		"outcome":   "clean",
		"note":      "no credentials",
	})
	if !strings.Contains(out, "recorded cors clean against https://example.com") {
		t.Errorf("the record was not confirmed:\n%s", out)
	}
	out = runLedger(t, map[string]any{"mode": "list"})
	if !strings.Contains(out, "coverage: 1 entries") {
		t.Errorf("the ledger did not come back:\n%s", out)
	}
	if !strings.Contains(out, "cors clean https://example.com") {
		t.Errorf("the entry is missing:\n%s", out)
	}
	if !strings.Contains(out, "origin header") || !strings.Contains(out, "no credentials") {
		t.Errorf("the detail and the note are missing:\n%s", out)
	}
}

// TestLedgerListReadsTheReceiptTrail: the merged list holds both ledgers, and
// each of them under its own heading.
func TestLedgerListReadsTheReceiptTrail(t *testing.T) {
	home := ledgerHome(t)
	writeReceipt(t, home, "r1", "https://example.com/admin", "GET", 200)
	writeReceipt(t, home, "r2", "https://example.com/login", "POST", 403)
	out := runLedger(t, map[string]any{"mode": "list"})
	if !strings.Contains(out, "history: 2 entries") {
		t.Errorf("the trail did not come back:\n%s", out)
	}
	if !strings.Contains(out, "GET https://example.com/admin status 200") {
		t.Errorf("the request is missing:\n%s", out)
	}
	if !strings.Contains(out, "receipt r2") {
		t.Errorf("the receipt id is missing:\n%s", out)
	}
	if !strings.Contains(out, "coverage: 0 entries") {
		t.Errorf("the coverage ledger is missing from the listing:\n%s", out)
	}
}

// TestLedgerListFiltersTheTrail: the trail filters belong to the trail.
func TestLedgerListFiltersTheTrail(t *testing.T) {
	home := ledgerHome(t)
	writeReceipt(t, home, "r1", "https://example.com/admin", "GET", 200)
	writeReceipt(t, home, "r2", "https://example.com/login", "POST", 403)
	out := runLedger(t, map[string]any{"mode": "list", "host": "example.com", "method": "POST", "status": 403})
	if !strings.Contains(out, "history: 1 entries") {
		t.Errorf("the filter did not narrow the trail:\n%s", out)
	}
	if strings.Contains(out, "/admin") {
		t.Errorf("a request the filter excluded is in the output:\n%s", out)
	}
	if !strings.Contains(out, "/login") {
		t.Errorf("the matching request is missing:\n%s", out)
	}
}

// TestLedgerHostsListsTheHostsInTheTrail: the hosts are what the sweep has
// touched, and they come out of the same trail.
func TestLedgerHostsListsTheHostsInTheTrail(t *testing.T) {
	home := ledgerHome(t)
	writeReceipt(t, home, "r1", "https://example.com/admin", "GET", 200)
	writeReceipt(t, home, "r2", "https://other.test/login", "GET", 200)
	out := runLedger(t, map[string]any{"mode": "hosts"})
	if !strings.Contains(out, "example.com") || !strings.Contains(out, "other.test") {
		t.Errorf("the hosts are missing:\n%s", out)
	}
}

// TestLedgerGapsNamesTheUntriedTechniques: a technique that was tried is not
// a gap, and one that was not is.
func TestLedgerGapsNamesTheUntriedTechniques(t *testing.T) {
	ledgerHome(t)
	runLedger(t, map[string]any{
		"mode":      "record",
		"technique": "cors",
		"target":    "https://example.com",
		"outcome":   "clean",
	})
	out := runLedger(t, map[string]any{"mode": "gaps", "target": "https://example.com"})
	if !strings.Contains(out, "not tried against https://example.com") {
		t.Errorf("the gaps do not name the target:\n%s", out)
	}
	index := strings.LastIndex(out, ": ")
	if index < 0 {
		t.Fatalf("the gaps are not a list:\n%s", out)
	}
	for _, name := range strings.Split(out[index+2:], ", ") {
		if name == "cors" {
			t.Errorf("cors was tried and is still named as a gap:\n%s", out)
		}
	}
	if !strings.Contains(out[index+2:], "idor") {
		t.Errorf("an untried technique is missing from the gaps:\n%s", out)
	}
}

// TestLedgerCatalogueListsTheChecklist: the catalogue is the list a sweep is
// expected to cover, and it is a list, not a judgement.
func TestLedgerCatalogueListsTheChecklist(t *testing.T) {
	ledgerHome(t)
	out := runLedger(t, map[string]any{"mode": "catalogue"})
	if !strings.HasPrefix(out, "catalogue: ") {
		t.Errorf("the catalogue is not labelled:\n%s", out)
	}
	if !strings.Contains(out, "idor") || !strings.Contains(out, "cors_misconfig") {
		t.Errorf("the checklist is missing techniques:\n%s", out)
	}
}

// TestLedgerReplayRefusesAnUnknownReceipt: a replay of a request that was
// never recorded is refused before anything is sent.
func TestLedgerReplayRefusesAnUnknownReceipt(t *testing.T) {
	ledgerHome(t)
	err := ledgerFails(t, map[string]any{"mode": "replay", "id": "nope"})
	if !strings.Contains(err.Error(), "no recorded request with id") {
		t.Errorf("the error does not name the cause: %v", err)
	}
}

// TestLedgerRunContextRefusesAnUnknownMode: the dispatch has to name the mode
// it does not know, because a mode writes a record or sends a request.
func TestLedgerRunContextRefusesAnUnknownMode(t *testing.T) {
	tl := &LedgerTool{}
	if _, err := tl.RunContext(t.Context(), operation.Spec{Type: "ledger", Args: map[string]any{"mode": "burn"}}); err == nil {
		t.Error("RunContext accepted an unknown mode")
	}
	if _, err := tl.RunContext(t.Context(), operation.Spec{Type: "ledger", Args: map[string]any{}}); err == nil {
		t.Error("RunContext accepted a spec with no mode")
	}
}

// TestLedgerTranslateResultPassesOutput: result translation is a pass through
// of the recorded output.
func TestLedgerTranslateResultPassesOutput(t *testing.T) {
	tl := &LedgerTool{}
	op := operation.Operation{Output: "recorded cors clean against https://example.com"}
	if got := tl.TranslateResult(op); got != op.Output {
		t.Errorf("TranslateResult = %q, want passthrough", got)
	}
}
