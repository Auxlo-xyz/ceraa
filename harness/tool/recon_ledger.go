package tool

// recon_ledger.go is the memory of a sweep wearing a tool face.
//
// Two tools became one. The coverage ledger recorded what a sweep had tried
// and named the techniques it had not; the receipt trail recorded what it had
// sent and could send it again with one thing changed. Both are the memory of
// a sweep, and both were two catalogue entries the model reads on every turn.
//
// The engines are untouched. harness/coverage still owns the ledger, and
// harness/history still owns the trail, the gate and the probe. This file
// shapes the arguments and dispatches on the mode, and every mode keeps its
// own output, because a coverage entry and a receipt are different facts and
// a merged rendering would blur the difference between them.
//
// The two tools had a list each, and one name can only be one mode. The
// merged list reads both ledgers, each under its own heading, and the filters
// belong to the ledger they filter: technique and target to the coverage
// entries, host and path and method and status and limit to the trail.

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/coverage"
	"ceraa/harness/history"
	"ceraa/harness/operation"
	"ceraa/internal/paths"
)

// LedgerTool keeps what a sweep tried and what it sent.
type LedgerTool struct{}

// The executor resolves a tool by its operation type and calls it through the
// context runner, so the two have to agree with the name.
var _ operation.ContextRunner = (*LedgerTool)(nil)

// The modes the one tool carries.
//
// record, list, gaps and catalogue read and write the coverage store; replay
// and hosts read the receipt trail, and replay sends the recorded request
// again through the probe.
const (
	ledgerModeRecord    = "record"
	ledgerModeList      = "list"
	ledgerModeGaps      = "gaps"
	ledgerModeCatalogue = "catalogue"
	ledgerModeReplay    = "replay"
	ledgerModeHosts     = "hosts"
)

const (
	// maxLedgerEntries bounds the coverage listing. A sweep records one entry
	// per technique per target, so the cap sits far above a normal ledger,
	// and a ledger past it says so rather than filling the context.
	maxLedgerEntries = 200
	// ledgerDefaultLimit and ledgerMaxLimit bound the receipt listing.
	ledgerDefaultLimit = 50
	ledgerMaxLimit     = 500
)

func (l *LedgerTool) Name() string { return "ledger" }

func (l *LedgerTool) Description() string {
	return "Keep the memory of a sweep: record what a technique was tried against, read the coverage ledger and the techniques it has not covered, and read the receipt trail or replay a recorded request with one thing changed."
}

func (l *LedgerTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{
				"type":        "string",
				"enum":        []string{ledgerModeRecord, ledgerModeList, ledgerModeGaps, ledgerModeCatalogue, ledgerModeReplay, ledgerModeHosts},
				"description": "record an attempt, list the coverage ledger and the receipt trail, gaps the catalogue techniques not tried against a target, catalogue the checklist, replay a recorded request with one thing changed, hosts in the trail",
			},

			// The coverage arguments.
			"technique": map[string]any{"type": "string", "description": "record and list: the technique tried, as cors or idor"},
			"target":    map[string]any{"type": "string", "description": "the coverage target the attempt was made against; record, list and gaps"},
			"detail":    map[string]any{"type": "string", "description": "record: how it was tried"},
			"outcome":   map[string]any{"type": "string", "enum": []string{"clean", "finding", "blocked", "skipped"}, "description": "record: clean, finding, blocked or skipped"},
			"note":      map[string]any{"type": "string", "description": "record: a note worth remembering"},

			// The receipt trail arguments.
			"id":             map[string]any{"type": "string", "description": "replay: the receipt id of the recorded request"},
			"host":           map[string]any{"type": "string", "description": "list: only this host"},
			"path_contains":  map[string]any{"type": "string", "description": "list: only urls containing this"},
			"method":         map[string]any{"type": "string", "description": "the history filter, only requests with this method; list"},
			"status":         map[string]any{"type": "integer", "description": "list: only this status"},
			"limit":          map[string]any{"type": "integer", "description": "list: entries to return, default 50"},
			"new_method":     map[string]any{"type": "string", "description": "replay: the method to send"},
			"new_url":        map[string]any{"type": "string", "description": "replay: the url to send"},
			"body":           map[string]any{"type": "string", "description": "replay: the body to send"},
			"headers":        map[string]any{"type": "object", "additionalProperties": true, "description": "replay: headers to add"},
			"remove_headers": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "replay: headers to drop"},
			"query":          map[string]any{"type": "object", "additionalProperties": true, "description": "replay: query parameters to set"},
		},
		// Nothing is required by the schema: the mode decides. A technique and
		// a target are required to record an attempt and an id to replay one,
		// and the tool body refuses the call that is short of one, because a
		// schema cannot make a field required in one mode only.
		"required": []string{},
	}
}

// Translate validates the shape against the mode and re-emits the canonical
// argument map.
func (l *LedgerTool) Translate(args map[string]any) (operation.Spec, error) {
	mode, err := ledgerMode(args["mode"])
	if err != nil {
		return operation.Spec{}, err
	}
	out := map[string]any{"mode": mode}

	switch mode {
	case ledgerModeRecord:
		technique := strings.TrimSpace(ledgerStringArg(args["technique"]))
		if technique == "" {
			return operation.Spec{}, fmt.Errorf("mode record needs a technique")
		}
		target := strings.TrimSpace(ledgerStringArg(args["target"]))
		if target == "" {
			return operation.Spec{}, fmt.Errorf("mode record needs a target")
		}
		out["technique"] = technique
		out["target"] = target
		for _, key := range []string{"detail", "outcome", "note"} {
			if value := ledgerStringArg(args[key]); value != "" {
				out[key] = value
			}
		}

	case ledgerModeList:
		// The two filters belong to the two ledgers: technique and target
		// narrow the coverage entries, the rest narrow the receipt trail.
		for _, key := range []string{"technique", "target", "host", "path_contains", "method"} {
			if value := ledgerStringArg(args[key]); value != "" {
				out[key] = value
			}
		}
		if status, ok := ledgerIntArg(args["status"]); ok {
			out["status"] = status
		}
		if limit, ok := ledgerIntArg(args["limit"]); ok && limit > 0 {
			out["limit"] = limit
		}

	case ledgerModeGaps:
		if target := strings.TrimSpace(ledgerStringArg(args["target"])); target != "" {
			out["target"] = target
		}

	case ledgerModeCatalogue, ledgerModeHosts:
		// Nothing to carry: the mode names the whole call.

	case ledgerModeReplay:
		id := strings.TrimSpace(ledgerStringArg(args["id"]))
		if id == "" {
			return operation.Spec{}, fmt.Errorf("mode replay needs the id of a recorded request")
		}
		out["id"] = id
		for _, key := range []string{"new_method", "new_url", "body"} {
			if value := ledgerStringArg(args[key]); value != "" {
				out[key] = value
			}
		}
		if headers, ok := args["headers"].(map[string]any); ok && len(headers) > 0 {
			out["headers"] = headers
		}
		if list := ledgerStringListArg(args["remove_headers"]); len(list) > 0 {
			out["remove_headers"] = list
		}
		if query, ok := args["query"].(map[string]any); ok && len(query) > 0 {
			out["query"] = query
		}
	}

	return operation.Spec{Type: "ledger", Args: out}, nil
}

func (l *LedgerTool) TranslateResult(op operation.Operation) string { return op.Output }

func (l *LedgerTool) Run(spec operation.Spec) (string, error) {
	return l.RunContext(context.Background(), spec)
}

// RunContext dispatches on the mode.
//
// The mode is the only thing that decides which store is read. Everything the
// store needs came out of Translate, so the two halves of the tool cannot
// drift apart: a mode that gets a missing argument is refused at the
// translation, not half-way through a write.
func (l *LedgerTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	mode, _ := spec.Args["mode"].(string)
	switch mode {
	case ledgerModeRecord:
		return l.record(spec)
	case ledgerModeList:
		return l.list(spec)
	case ledgerModeGaps:
		return l.gaps(spec)
	case ledgerModeCatalogue:
		return "catalogue: " + strings.Join(coverage.Catalogue(), ", "), nil
	case ledgerModeReplay:
		return l.replay(ctx, spec)
	case ledgerModeHosts:
		hosts, err := history.Hosts(ledgerReceiptsDir())
		if err != nil {
			return "", err
		}
		return "hosts: " + strings.Join(hosts, ", "), nil
	}
	return "", fmt.Errorf("mode %q is not one of record, list, gaps, catalogue, replay, hosts", mode)
}

// record writes one attempt into the coverage ledger.
func (l *LedgerTool) record(spec operation.Spec) (string, error) {
	store, err := coverage.Open(ledgerCoverageDir())
	if err != nil {
		return "", err
	}
	entry := coverage.Entry{
		Technique: ledgerStringArg(spec.Args["technique"]),
		Target:    ledgerStringArg(spec.Args["target"]),
		Detail:    ledgerStringArg(spec.Args["detail"]),
		Outcome:   coverage.Outcome(ledgerStringArg(spec.Args["outcome"])),
		Note:      ledgerStringArg(spec.Args["note"]),
	}
	saved, err := store.Record(entry)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("recorded %s %s against %s", saved.Technique, saved.Outcome, saved.Target), nil
}

// list reads both ledgers: the coverage entries and the receipt trail.
//
// The two are separate sections because they are separate facts. A coverage
// entry says what was tried against what; a receipt says what was sent and
// what came back, and the two are read with different filters.
func (l *LedgerTool) list(spec operation.Spec) (string, error) {
	entries, err := l.coverageEntries(spec)
	if err != nil {
		return "", err
	}
	trail, err := l.trailEntries(spec)
	if err != nil {
		return "", err
	}
	return entries + "\n" + trail, nil
}

// coverageEntries prints the recorded attempts.
func (l *LedgerTool) coverageEntries(spec operation.Spec) (string, error) {
	store, err := coverage.Open(ledgerCoverageDir())
	if err != nil {
		return "", err
	}
	technique := ledgerStringArg(spec.Args["technique"])
	target := ledgerStringArg(spec.Args["target"])
	entries, err := store.List(technique, target)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "coverage: %d entries\n", len(entries))
	for i, entry := range entries {
		if i >= maxLedgerEntries {
			fmt.Fprintf(&b, "omitting %d further entries: the %d entry cap was reached\n", len(entries)-maxLedgerEntries, maxLedgerEntries)
			break
		}
		fmt.Fprintf(&b, "%s %s %s %s", entry.Technique, entry.Outcome, entry.Target, entry.At.Format("2006-01-02T15:04:05Z"))
		if entry.Detail != "" {
			fmt.Fprintf(&b, " - %s", entry.Detail)
		}
		if entry.Note != "" {
			fmt.Fprintf(&b, " | %s", entry.Note)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// trailEntries prints the recorded requests.
func (l *LedgerTool) trailEntries(spec operation.Spec) (string, error) {
	filter := history.Filter{
		Host:         ledgerStringArg(spec.Args["host"]),
		PathContains: ledgerStringArg(spec.Args["path_contains"]),
		Method:       ledgerStringArg(spec.Args["method"]),
		Limit:        ledgerDefaultLimit,
	}
	if status, ok := spec.Args["status"].(int); ok {
		filter.Status = status
	}
	if limit, ok := spec.Args["limit"].(int); ok && limit > 0 {
		if limit > ledgerMaxLimit {
			limit = ledgerMaxLimit
		}
		filter.Limit = limit
	}
	entries, err := history.Query(ledgerReceiptsDir(), filter)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "history: %d entries\n", len(entries))
	for _, entry := range entries {
		fmt.Fprintf(&b, "%s %s %s status %d sha256:%s receipt %s",
			entry.At.Format("2006-01-02T15:04:05Z"),
			entry.Method,
			entry.URL,
			entry.Status,
			ledgerShortHash(entry.SHA256),
			entry.ID)
		if entry.RedirectedTo != "" {
			fmt.Fprintf(&b, " redirect %s", entry.RedirectedTo)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// gaps names the catalogue techniques that were never tried against a target.
func (l *LedgerTool) gaps(spec operation.Spec) (string, error) {
	store, err := coverage.Open(ledgerCoverageDir())
	if err != nil {
		return "", err
	}
	target := ledgerStringArg(spec.Args["target"])
	gaps, err := store.Gaps(target, nil)
	if err != nil {
		return "", err
	}
	if len(gaps) == 0 {
		return "coverage: every technique in the catalogue was tried against " + target, nil
	}
	return fmt.Sprintf("coverage: %d techniques not tried against %s: %s", len(gaps), target, strings.Join(gaps, ", ")), nil
}

// replay sends a recorded request again with the change applied.
func (l *LedgerTool) replay(ctx context.Context, spec operation.Spec) (string, error) {
	id := ledgerStringArg(spec.Args["id"])
	entries, err := history.Query(ledgerReceiptsDir(), history.Filter{})
	if err != nil {
		return "", err
	}
	var entry history.Entry
	found := false
	for _, candidate := range entries {
		if candidate.ID == id {
			entry = candidate
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("no recorded request with id %q", id)
	}
	change := history.Change{
		Method: ledgerStringArg(spec.Args["new_method"]),
		URL:    ledgerStringArg(spec.Args["new_url"]),
		Body:   ledgerStringArg(spec.Args["body"]),
	}
	if headers, ok := spec.Args["headers"].(map[string]any); ok {
		change.Headers = map[string]string{}
		for name, value := range headers {
			switch typed := value.(type) {
			case string:
				change.Headers[name] = typed
			case bool, int, float64:
				change.Headers[name] = fmt.Sprint(typed)
			}
		}
	}
	for _, name := range ledgerStringListArg(spec.Args["remove_headers"]) {
		change.RemoveHeaders = append(change.RemoveHeaders, name)
	}
	if query, ok := spec.Args["query"].(map[string]any); ok {
		change.Query = map[string]string{}
		for name, value := range query {
			switch typed := value.(type) {
			case string:
				change.Query[name] = typed
			case bool, int, float64:
				change.Query[name] = fmt.Sprint(typed)
			}
		}
	}
	result, err := history.Replay(ctx, entry, change)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "replay %s %s\n", result.Method, result.URL)
	fmt.Fprintf(&b, "status %d in %dms, body %dB, sha256:%s\n", result.Status, result.ElapsedMS, result.BodyBytes, ledgerShortHash(result.SHA256))
	if result.RedirectedTo != "" {
		fmt.Fprintf(&b, "redirect %s (not followed)\n", result.RedirectedTo)
	}
	if result.ReceiptID != "" {
		fmt.Fprintf(&b, "receipt %s\n", result.ReceiptID)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ledgerMode reads the mode argument.
//
// The default is record, because a sweep that names a technique without saying
// what to do with it is recording one.
func ledgerMode(value any) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(ledgerStringArg(value)))
	if mode == "" {
		return ledgerModeRecord, nil
	}
	switch mode {
	case ledgerModeRecord, ledgerModeList, ledgerModeGaps, ledgerModeCatalogue, ledgerModeReplay, ledgerModeHosts:
		return mode, nil
	}
	return "", fmt.Errorf("mode %q is not one of record, list, gaps, catalogue, replay, hosts", mode)
}

// ledgerStringArg reads a string argument without the zero-value trap.
func ledgerStringArg(value any) string {
	text, _ := value.(string)
	return text
}

// ledgerIntArg reads an integer argument in the native shape and in the
// JSON-decoded one, because a number that arrives from the model is a float.
func ledgerIntArg(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case float64:
		return int(typed), true
	}
	return 0, false
}

// ledgerStringListArg reads a list of strings in both shapes.
func ledgerStringListArg(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			if text, ok := entry.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

// ledgerShortHash trims a hash for display.
func ledgerShortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// ledgerCoverageDir is where the coverage ledger lives.
func ledgerCoverageDir() string {
	return paths.DataDir() + "/coverage"
}

// ledgerReceiptsDir is where the probe writes its receipts.
func ledgerReceiptsDir() string {
	return paths.DataDir() + "/recon/receipts"
}

// The names this tool had before the merge.
//
