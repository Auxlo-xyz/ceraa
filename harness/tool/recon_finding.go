package tool

// recon_finding.go is the finding record wearing a tool face, with the
// verification policy folded into it.
//
// The store it writes is the harness/findings package; this file is only the
// shape the model talks to. Three things are decided here rather than there:
//
//   - what a mode does. add records, update changes the fields the model
//     named and leaves the rest alone, list and coverage read, delete
//     removes, and verify runs the verification policy over an exchange and
//     attaches the verdict to the record. An update that defaulted the
//     severity would overwrite the severity the record already carried, which
//     is how a record quietly stops describing what it describes.
//   - what the chat sees. One finding per block, and the reproduction
//     printed verbatim, because the PoC is the thing a reader copies and a
//     copy that needs un-escaping is a copy that gets pasted wrong.
//   - that verifying a finding is a finding operation. It was a second
//     catalogue entry, and the catalogue is the cached prefix of every
//     request, so two schemas for one job are two schemas the model reads on
//     every turn. The policy stays where it was, in harness/verify, pure and
//     fetching nothing; this file is the part that goes and gets the
//     exchanges.

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"ceraa/harness/findings"
	"ceraa/harness/oast"
	"ceraa/harness/operation"
	"ceraa/harness/recon"
	"ceraa/harness/verify"
	"ceraa/internal/paths"
)

// The six behaviours the one tool carries.
//
// The modes are separate rather than one mode with flags because each has its
// own argument set: a verify names an exchange, an add names a title, and a
// schema cannot make a field conditionally required. The mode decides what
// the call must carry and the body refuses a call that is short of it.
const (
	findingModeAdd      = "add"
	findingModeUpdate   = "update"
	findingModeList     = "list"
	findingModeDelete   = "delete"
	findingModeCoverage = "coverage"
	findingModeVerify   = "verify"
)

// FindingTool records what a recon run found, ruled out and left open, and
// runs the verification policy over an exchange.
type FindingTool struct{}

// VerifyTool is the name the registry still registers.

// The executor resolves a tool by its operation type and calls it through the
// context runner, so the two have to agree with the name.
var _ operation.ContextRunner = (*FindingTool)(nil)

func (f *FindingTool) Name() string { return "finding" }

func (f *FindingTool) Description() string {
	return "Keep the finding record: confirmed, ruled_out and open_proof_gap, each with its PoC, evidence, counterargument and CVSS, all three rendered by the report, and verify an exchange for a PROVEN, REFUTED, UNPROVEN or INCONCLUSIVE verdict."
}

func (f *FindingTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{
				"type":        "string",
				"enum":        []string{findingModeAdd, findingModeUpdate, findingModeList, findingModeDelete, findingModeCoverage, findingModeVerify},
				"description": "add, update, list, delete, coverage or verify",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "add or update: the finding id, generated from the title when absent; verify: the finding to attach the verdict to",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "What the finding is, in one line",
			},
			"severity": map[string]any{
				"type":        "string",
				"enum":        []string{"info", "low", "medium", "high", "critical"},
				"description": "info, low, medium, high or critical",
			},
			"status": map[string]any{
				// The type is left open on purpose: the argument carries the
				// closure state for add and update and the response status for
				// verify, and one property cannot be two types. The body
				// refuses each mode's reading of the other's value.
				"additionalProperties": true,
				"description":          "add or update: the closure state, confirmed, ruled_out or open_proof_gap; verify: the response status as a number",
			},
			"url": map[string]any{
				"type":        "string",
				"description": "add or update: the finding's URL; verify: the exchange to verify",
			},
			"endpoint":    map[string]any{"type": "string", "description": "The endpoint"},
			"description": map[string]any{"type": "string", "description": "What it is and why it matters"},
			"poc":         map[string]any{"type": "string", "description": "The reproduction, verbatim"},
			"evidence":    map[string]any{"type": "string", "description": "What was observed"},
			"counterevidence": map[string]any{
				"type":        "string",
				"description": "What argues against it",
			},
			"remediation": map[string]any{"type": "string", "description": "What would fix it"},
			"cvss": map[string]any{
				"type":        "string",
				"description": "A CVSS v3.1 vector or the eight metrics",
			},
			"tags": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"maxItems":    12,
				"description": "Labels for the report",
			},
			"method": map[string]any{
				"type":        "string",
				"description": "verify: the method, default GET",
			},
			"request_body": map[string]any{
				"type":        "string",
				"description": "verify: the request body",
			},
			"response_body": map[string]any{
				"type":        "string",
				"description": "verify: the response body, if known",
			},
			"baseline_url": map[string]any{
				"type":        "string",
				"description": "verify: the baseline url, fetched when given",
			},
			"baseline_status": map[string]any{
				"type":        "integer",
				"description": "verify: the baseline status, if known",
			},
			"baseline_body": map[string]any{
				"type":        "string",
				"description": "verify: the baseline body, if known",
			},
			"marker": map[string]any{
				"type":        "string",
				"description": "verify: the canary that was injected",
			},
			"controls": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "verify: controls to run: marker, status, body, reflection, secret, callback (a blind finding proven by an interaction at its oast address)",
			},
		},
		// Nothing is required by the schema: the mode decides. A title is
		// required to add or update a finding, an id to delete one, a url to
		// verify an exchange, and listing the record needs neither.
		"required": []string{},
	}
}

// Translate validates the shape and re-emits the canonical argument map.
func (f *FindingTool) Translate(args map[string]any) (operation.Spec, error) {
	mode, err := findingMode(args["mode"])
	if err != nil {
		return operation.Spec{}, err
	}
	out := map[string]any{"mode": mode}

	switch mode {
	case findingModeAdd, findingModeUpdate:
		title := strings.TrimSpace(findingString(args["title"]))
		if title == "" {
			return operation.Spec{}, fmt.Errorf("title is required")
		}
		out["title"] = title
		// The id comes from the title when the model does not name one, so a
		// second call about the same finding lands on the same record
		// instead of on a second copy of it.
		if id := strings.TrimSpace(findingString(args["id"])); id != "" {
			out["id"] = id
		} else {
			out["id"] = findings.Slug(title)
		}
		// The defaults belong to a new finding only. An update that did not
		// name a severity keeps the one the record carries, which is the
		// whole difference between an update and a rewrite.
		if severity := strings.TrimSpace(findingString(args["severity"])); severity != "" {
			parsed, err := findings.ParseSeverity(severity)
			if err != nil {
				return operation.Spec{}, err
			}
			out["severity"] = parsed
		} else if mode == findingModeAdd {
			out["severity"] = findings.SeverityMedium
		}
		status, err := findingClosureStatus(args["status"], mode)
		if err != nil {
			return operation.Spec{}, err
		}
		if status != "" {
			out["status"] = status
		}
		for _, field := range []string{"url", "endpoint", "description", "poc", "evidence", "counterevidence", "remediation"} {
			if value := findingString(args[field]); strings.TrimSpace(value) != "" {
				out[field] = value
			}
		}
		tags, err := findingTags(args["tags"])
		if err != nil {
			return operation.Spec{}, err
		}
		if len(tags) > 0 {
			out["tags"] = tags
		}
		vector, score, err := findingCVSS(args["cvss"])
		if err != nil {
			return operation.Spec{}, err
		}
		if vector != "" {
			out["cvss_vector"] = vector
			out["cvss_score"] = score
		}

	case findingModeDelete:
		id := strings.TrimSpace(findingString(args["id"]))
		if id == "" {
			return operation.Spec{}, fmt.Errorf("id is required")
		}
		out["id"] = id

	case findingModeVerify:
		url := strings.TrimSpace(findingString(args["url"]))
		if url == "" {
			return operation.Spec{}, fmt.Errorf("url is required")
		}
		out["url"] = url
		// The id names the finding the verdict is attached to. It is
		// optional: a verdict that proves a finding nobody recorded yet is
		// still a verdict worth reading.
		if id := strings.TrimSpace(findingString(args["id"])); id != "" {
			out["id"] = id
		}
		for _, key := range []string{"method", "request_body", "response_body", "baseline_url", "baseline_body", "marker"} {
			if value := findingString(args[key]); strings.TrimSpace(value) != "" {
				out[key] = value
			}
		}
		status, hasStatus, err := findingResponseStatus(args["status"])
		if err != nil {
			return operation.Spec{}, err
		}
		if hasStatus {
			out["status"] = status
		}
		baseline, hasBaseline, err := findingResponseStatus(args["baseline_status"])
		if err != nil {
			return operation.Spec{}, err
		}
		if hasBaseline {
			out["baseline_status"] = baseline
		}
		controls := findingControlNames(args["controls"])
		if len(controls) > 0 {
			out["controls"] = controls
		}
	}

	return operation.Spec{Type: "finding", Args: out}, nil
}

func (f *FindingTool) TranslateResult(op operation.Operation) string {
	return op.Output
}

func (f *FindingTool) Run(spec operation.Spec) (string, error) {
	return f.RunContext(context.Background(), spec)
}

// RunContext carries the record.
//
// The store is opened per call rather than held open: the directory it lives
// in is under the data directory, and a long-lived handle would keep writing
// into a directory the operator moved.
func (f *FindingTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	mode, _ := spec.Args["mode"].(string)

	// The verification opens the store only when it has a record to attach
	// the verdict to, so it returns before the store is created.
	if mode == findingModeVerify {
		return f.runVerify(ctx, spec)
	}

	store, err := findings.Open(findingsDir())
	if err != nil {
		return "", err
	}

	switch mode {
	case findingModeAdd:
		stored, err := store.Put(findingFromArgs(spec.Args, findings.Finding{}))
		if err != nil {
			return "", err
		}
		return renderFindingBlock("recorded", stored), nil

	case findingModeUpdate:
		id, _ := spec.Args["id"].(string)
		if err := findings.ValidateID(id); err != nil {
			return "", err
		}
		existing, ok := store.Get(id)
		if !ok {
			return "", fmt.Errorf("no finding with id %q", id)
		}
		stored, err := store.Put(findingFromArgs(spec.Args, existing))
		if err != nil {
			return "", err
		}
		return renderFindingBlock("updated", stored), nil

	case findingModeList:
		return renderFindingList(store)

	case findingModeDelete:
		id, _ := spec.Args["id"].(string)
		if err := findings.ValidateID(id); err != nil {
			return "", err
		}
		if err := store.Delete(id); err != nil {
			return "", err
		}
		return fmt.Sprintf("deleted finding %s\n", id), nil

	case findingModeCoverage:
		return renderFindingCoverage(store)
	}
	return "", fmt.Errorf("mode %q is not one of add, update, list, delete, coverage, verify", mode)
}

// Sequential keeps the record's writes in order. Two calls in one batch are
// one model turn, and a batch that records a finding and then updates it has
// to run in that order.
func (f *FindingTool) Sequential() bool { return true }

// SequentialQueue puts the record on the file lane, with the other tools
// that write to disk.
func (f *FindingTool) SequentialQueue() string { return "file" }

// MutationKey groups the calls that touch the same finding, so a record
// cannot be rewritten while another write to it is in flight.
//
// It is the finding's file rather than a bare id: the key is a path
// everywhere else in the harness, and a key that is only a name would be read
// as a relative path by anything that resolves one.
func (f *FindingTool) MutationKey(args map[string]any) string {
	id := strings.TrimSpace(findingString(args["id"]))
	if id == "" {
		title := strings.TrimSpace(findingString(args["title"]))
		if title == "" {
			return ""
		}
		id = findings.Slug(title)
	}
	if err := findings.ValidateID(id); err != nil {
		return ""
	}
	return filepath.Join(findingsDir(), id+".json")
}

// findingsDir is where the record lives. It is resolved per call rather than
// captured at start-up, because the data directory honours CERAA_HOME and a
// process that read it once would keep answering with the first value it saw.
func findingsDir() string {
	return filepath.Join(paths.DataDir(), "findings")
}

// findingMode reads the mode argument.
func findingMode(value any) (string, error) {
	mode := strings.TrimSpace(findingString(value))
	if mode == "" {
		return findingModeAdd, nil
	}
	switch mode {
	case findingModeAdd, findingModeUpdate, findingModeList, findingModeDelete, findingModeCoverage, findingModeVerify:
		return mode, nil
	}
	return "", fmt.Errorf("mode %q is not one of add, update, list, delete, coverage, verify", mode)
}

// findingString reads a string argument, treating any other type as absent.
func findingString(value any) string {
	text, _ := value.(string)
	return text
}

// findingStatus reads the closure state out of the argument map.
//
// The spec carries it as a findings.Status, and a plain string is accepted
// too, because the two arrive from different places: the translator writes
// the typed value and a caller building a spec by hand writes the text.
func findingStatus(value any) findings.Status {
	switch typed := value.(type) {
	case findings.Status:
		return typed
	case string:
		return findings.Status(typed)
	}
	return ""
}

// findingClosureStatus reads the closure state for add and update.
//
// The status argument is shared with the verify mode, where it carries a
// response status, so a number arriving here is a mistake: it is refused by
// name rather than read as an absent state and defaulted, because a finding
// that silently took the default closure state is a finding nobody chose.
func findingClosureStatus(value any, mode string) (findings.Status, error) {
	if value == nil {
		return findingDefaultStatus(mode), nil
	}
	switch typed := value.(type) {
	case findings.Status:
		if strings.TrimSpace(string(typed)) == "" {
			return findingDefaultStatus(mode), nil
		}
		return findings.ParseStatus(string(typed))
	case string:
		if strings.TrimSpace(typed) == "" {
			return findingDefaultStatus(mode), nil
		}
		return findings.ParseStatus(typed)
	case map[string]any:
		// Models that cannot express a union sometimes send the state as a
		// tagged map, {"open_proof_gap": true}. The reading is unambiguous, so
		// take it rather than failing the call and spending a turn on a retry.
		if state, ok := singleTaggedStatus(typed); ok {
			return findings.ParseStatus(state)
		}
	}
	return "", fmt.Errorf("status for %s is the closure state %s, as a plain string; got %s. A response status is only valid in verify mode",
		mode, statusChoices(), statusValueKind(value))
}

// singleTaggedStatus reads {"<state>": true} as the state. Anything else is
// left to the error, which names the shape it wanted.
func singleTaggedStatus(value map[string]any) (string, bool) {
	if len(value) != 1 {
		return "", false
	}
	for state, marked := range value {
		on, _ := marked.(bool)
		if !on {
			return "", false
		}
		state = strings.TrimSpace(state)
		if _, err := findings.ParseStatus(state); err != nil {
			return "", false
		}
		return state, true
	}
	return "", false
}

func statusChoices() string {
	return "confirmed, ruled_out or open_proof_gap"
}

func statusValueKind(value any) string {
	switch value.(type) {
	case map[string]any:
		return "an object"
	case float64, int, int64:
		return "a number"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// findingDefaultStatus is the state a new finding starts in. An update keeps
// the state the record carries.
func findingDefaultStatus(mode string) findings.Status {
	if mode == findingModeAdd {
		return findings.StatusConfirmed
	}
	return ""
}

// findingResponseStatus reads a response status for the verify mode.
//
// The number arrives from a JSON body as a float, and a model that writes
// it as text sends a string, so both are read. The boolean says whether a
// status was given at all: a status of zero is not a status, and the caller
// fetches the exchange when the answer was left out.
func findingResponseStatus(value any) (int, bool, error) {
	switch typed := value.(type) {
	case nil:
		return 0, false, nil
	case int:
		return typed, true, nil
	case float64:
		if typed != float64(int(typed)) {
			return 0, false, fmt.Errorf("status %v is not a whole number", typed)
		}
		return int(typed), true, nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false, nil
		}
		parsed, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, false, fmt.Errorf("status %q is not a number", typed)
		}
		return parsed, true, nil
	}
	return 0, false, fmt.Errorf("status is not a number")
}

// maxFindingTags bounds the labels one finding may carry. A finding with
// thirty tags is a finding nobody can group.
const maxFindingTags = 12

// findingTags reads the tags argument in both native and JSON-decoded shapes.
func findingTags(value any) ([]string, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case []string:
		return findingTagList(typed)
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			text, ok := entry.(string)
			if !ok {
				return nil, fmt.Errorf("tags must be an array of strings")
			}
			out = append(out, text)
		}
		return findingTagList(out)
	default:
		return nil, fmt.Errorf("tags must be an array of strings")
	}
}

// findingTagList trims the tags and bounds how many there may be.
func findingTagList(tags []string) ([]string, error) {
	if len(tags) > maxFindingTags {
		return nil, fmt.Errorf("tags exceeded: %d given, maximum %d", len(tags), maxFindingTags)
	}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out, nil
}

// findingCVSS reads the cvss argument in the shapes a model sends it: a full
// vector, the eight metrics on their own, or an object of the eight metrics.
//
// The score is computed here rather than carried in, because a severity
// claim that quotes a vector nobody checked is exactly the claim a triage
// step is there to refuse.
func findingCVSS(value any) (string, float64, error) {
	switch typed := value.(type) {
	case nil:
		return "", 0, nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return "", 0, nil
		}
		metrics, err := findings.ParseVector(trimmed)
		if err != nil {
			return "", 0, err
		}
		score, err := metrics.Score()
		if err != nil {
			return "", 0, err
		}
		return metrics.Vector(), score, nil
	case map[string]any:
		metrics := findings.Metrics{}
		for key, raw := range typed {
			text, ok := raw.(string)
			if !ok {
				return "", 0, fmt.Errorf("cvss metric %q must be a string", key)
			}
			switch strings.ToUpper(strings.TrimSpace(key)) {
			case "AV":
				metrics.AV = text
			case "AC":
				metrics.AC = text
			case "PR":
				metrics.PR = text
			case "UI":
				metrics.UI = text
			case "S":
				metrics.S = text
			case "C":
				metrics.C = text
			case "I":
				metrics.I = text
			case "A":
				metrics.A = text
			default:
				return "", 0, fmt.Errorf("cvss metric %q is not one of AV, AC, PR, UI, S, C, I, A", key)
			}
		}
		score, err := metrics.Score()
		if err != nil {
			return "", 0, err
		}
		return metrics.Vector(), score, nil
	default:
		return "", 0, fmt.Errorf("cvss must be a vector string or an object of the eight metrics")
	}
}

// findingFromArgs builds the finding to write.
//
// For an update the base is the record as it stands and only the fields the
// model actually named are replaced. A field the model left out is not
// evidence that the field should be empty.
func findingFromArgs(args map[string]any, base findings.Finding) findings.Finding {
	stored := base
	if title := strings.TrimSpace(findingString(args["title"])); title != "" {
		stored.Title = title
	}
	if id := strings.TrimSpace(findingString(args["id"])); id != "" {
		stored.ID = id
	}
	if severity := findingString(args["severity"]); severity != "" {
		stored.Severity = severity
	}
	if status := findingStatus(args["status"]); status != "" {
		stored.Status = status
	}
	if value := findingString(args["url"]); strings.TrimSpace(value) != "" {
		stored.URL = value
	}
	if value := findingString(args["endpoint"]); strings.TrimSpace(value) != "" {
		stored.Endpoint = value
	}
	if value := findingString(args["description"]); strings.TrimSpace(value) != "" {
		stored.Description = value
	}
	if value := findingString(args["poc"]); strings.TrimSpace(value) != "" {
		stored.PoC = value
	}
	if value := findingString(args["evidence"]); strings.TrimSpace(value) != "" {
		stored.Evidence = value
	}
	if value := findingString(args["counterevidence"]); strings.TrimSpace(value) != "" {
		stored.Counterevidence = value
	}
	if value := findingString(args["remediation"]); strings.TrimSpace(value) != "" {
		stored.Remediation = value
	}
	if tags, ok := args["tags"].([]string); ok && len(tags) > 0 {
		stored.Tags = tags
	}
	if vector := findingString(args["cvss_vector"]); vector != "" {
		stored.CVSSVector = vector
	}
	if score, ok := args["cvss_score"].(float64); ok {
		stored.CVSSScore = score
	}
	return stored
}

// maxFindingLines bounds the list. A record with hundreds of findings is a
// record the reader has to page through, and the cap says so rather than
// truncating silently.
const maxFindingLines = 50

// renderFindingList prints one line per finding.
func renderFindingList(store *findings.Store) (string, error) {
	list, err := store.List()
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "no findings recorded\n", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "findings: %d\n", len(list))
	for i, f := range list {
		if i >= maxFindingLines {
			fmt.Fprintf(&b, "omitting %d further findings: the %d line cap was reached\n", len(list)-maxFindingLines, maxFindingLines)
			break
		}
		fmt.Fprintf(&b, "%s  %s  %s  %s  %s\n", f.ID, f.Severity, f.Status, findingOneLine(f.Title), f.URL)
	}
	return b.String(), nil
}

// renderFindingCoverage prints the counts and the open gaps.
func renderFindingCoverage(store *findings.Store) (string, error) {
	coverage, err := store.Coverage()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "findings: %d\n", coverage.Total)
	// All three states are named, including at zero: "confirmed: 0" is the
	// sentence a reader needs when nothing was proved.
	fmt.Fprintf(&b, "by status: confirmed %d, ruled_out %d, open_proof_gap %d\n",
		coverage.ByStatus[findings.StatusConfirmed],
		coverage.ByStatus[findings.StatusRuledOut],
		coverage.ByStatus[findings.StatusOpenProofGap])
	var severities []string
	for _, name := range []string{findings.SeverityCritical, findings.SeverityHigh, findings.SeverityMedium, findings.SeverityLow, findings.SeverityInfo} {
		if count := coverage.BySeverity[name]; count > 0 {
			severities = append(severities, fmt.Sprintf("%s %d", name, count))
		}
	}
	if len(severities) > 0 {
		fmt.Fprintf(&b, "by severity: %s\n", strings.Join(severities, ", "))
	}
	if len(coverage.OpenGaps) > 0 {
		b.WriteString("open proof gaps:\n")
		for _, title := range coverage.OpenGaps {
			fmt.Fprintf(&b, "- %s\n", findingOneLine(title))
		}
	}
	return b.String(), nil
}

// renderFindingBlock prints one finding as a block the chat can read.
//
// The reproduction is printed verbatim. It is the thing a reader copies, and
// a copy that needs un-escaping is a copy that gets pasted wrong.
func renderFindingBlock(verb string, f findings.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s finding %s\n", verb, f.ID)
	fmt.Fprintf(&b, "title: %s\n", findingOneLine(f.Title))
	fmt.Fprintf(&b, "severity: %s\n", f.Severity)
	fmt.Fprintf(&b, "status: %s\n", f.Status)
	if f.URL != "" {
		fmt.Fprintf(&b, "url: %s\n", f.URL)
	}
	if f.Endpoint != "" {
		fmt.Fprintf(&b, "endpoint: %s\n", findingOneLine(f.Endpoint))
	}
	if f.CVSSVector != "" || f.CVSSScore > 0 {
		fmt.Fprintf(&b, "cvss: %.1f (%s)\n", f.CVSSScore, f.CVSSVector)
	}
	if len(f.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(f.Tags, ", "))
	}
	if f.PoC != "" {
		b.WriteString("poc:\n")
		b.WriteString(findings.Fence(f.PoC))
		b.WriteString("\n")
	}
	return b.String()
}

// findingOneLine collapses a value onto one line, so a model-written title
// cannot break the block layout it is printed in.
func findingOneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// ---------------------------------------------------------------------------
// verify
// ---------------------------------------------------------------------------

// runVerify runs the verification policy over an exchange and attaches the
// verdict to the record.
//
// The policy itself lives in harness/verify and fetches nothing. This is the
// part that goes and gets the exchanges, so the policy stays pure and the
// network stays here, under the scope gate and the receipt trail.
func (f *FindingTool) runVerify(ctx context.Context, spec operation.Spec) (string, error) {
	url, _ := spec.Args["url"].(string)
	method, _ := spec.Args["method"].(string)
	if method == "" {
		method = "GET"
	}
	requestBody, _ := spec.Args["request_body"].(string)

	observation := verify.Observation{
		URL:         url,
		Method:      method,
		RequestBody: requestBody,
	}
	status, hasStatus := spec.Args["status"].(int)
	responseBody, hasBody := spec.Args["response_body"].(string)
	oobHost := ""
	if !hasStatus || !hasBody {
		result, body, err := f.fetch(ctx, url, method, requestBody)
		if err != nil {
			return "", err
		}
		oobHost = result.OOB
		if !hasStatus {
			observation.Status = result.Status
		} else {
			observation.Status = status
		}
		if !hasBody {
			observation.ResponseBody = string(body)
		} else {
			observation.ResponseBody = responseBody
		}
		observation.ResponseHeaders = result.Headers
	}

	baselineURL, _ := spec.Args["baseline_url"].(string)
	baselineBody, hasBaselineBody := spec.Args["baseline_body"].(string)
	baselineStatus, hasBaselineStatus := spec.Args["baseline_status"].(int)
	if baselineURL != "" || hasBaselineBody {
		baseline := &verify.Observation{URL: baselineURL, Method: method}
		if baselineURL != "" {
			result, body, err := f.fetch(ctx, baselineURL, method, "")
			if err != nil {
				return "", err
			}
			baseline.Status = result.Status
			baseline.ResponseBody = string(body)
			baseline.ResponseHeaders = result.Headers
		}
		if hasBaselineStatus {
			baseline.Status = baselineStatus
		}
		if hasBaselineBody {
			baseline.ResponseBody = baselineBody
		}
		observation.Baseline = baseline
	}

	controls, err := findingControls(spec.Args["controls"])
	if err != nil {
		return "", err
	}
	if wantsCallback(controls) {
		// The exchange that just went out may have minted its own address,
		// and the one the recorded probe used is in its receipt. Watching
		// both is what makes a copied URL as good as a re-injected marker.
		hosts := make([]string, 0, 2)
		if oobHost != "" {
			hosts = append(hosts, oobHost)
		}
		if host, ok := recon.ReceiptOOB(url, method); ok {
			hosts = append(hosts, host)
		}
		hosts = dedupe(hosts)
		if len(hosts) > 0 {
			observation.CallbackHost = strings.Join(hosts, ", ")
			interactions, err := oast.Await(ctx, hosts, callbackBudget)
			if err != nil {
				// A poll that did not run is an inconclusive check, not a
				// refutation, and the control's own obligation says so.
				observation.CallbackHost += " (poll failed: " + err.Error() + ")"
			} else {
				for _, interaction := range interactions {
					observation.Callbacks = append(observation.Callbacks, verify.Callback{
						Protocol:   interaction.Protocol,
						RemoteAddr: interaction.RemoteAddress,
						RawRequest: interaction.RawRequest,
						At:         interaction.Timestamp,
					})
				}
			}
		}
	}
	decision := verify.Decide(observation, controls)

	// The verdict is worth attaching to the record, because a finding whose
	// verdict lives only in a chat message is a finding that gets reported
	// without it.
	if id, _ := spec.Args["id"].(string); id != "" {
		store, err := findings.Open(findingsDir())
		if err != nil {
			return "", err
		}
		finding, ok := store.Get(id)
		if !ok {
			return "", fmt.Errorf("no finding with id %q", id)
		}
		finding.Verdict = string(decision.Verdict)
		finding.Reasons = decision.Reasons
		if _, err := store.Put(finding); err != nil {
			return "", err
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "verdict: %s\n", decision.Verdict)
	for _, obligation := range decision.Obligations {
		fmt.Fprintf(&b, "obligation %s: %s", obligation.Name, obligation.State)
		if obligation.Detail != "" {
			fmt.Fprintf(&b, " - %s", obligation.Detail)
		}
		if obligation.Evidence != "" {
			fmt.Fprintf(&b, " | %s", obligation.Evidence)
		}
		b.WriteString("\n")
	}
	for _, reason := range decision.Reasons {
		fmt.Fprintf(&b, "reason: %s\n", reason)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// callbackBudget is how long a blind verification waits for its interaction.
//
// Long enough for a target that shells out or resolves a name through a
// resolver cache, short enough that a finding with no callback does not hold
// the turn. A quiet window is inconclusive, never a refutation, so the number
// bounds patience rather than evidence.
const callbackBudget = 12 * time.Second

// wantsCallback reports whether the caller asked the callback control to run.
// The wait only happens when it was asked for: every other verification would
// otherwise pay twelve seconds for a question it did not ask.
func wantsCallback(controls []verify.Control) bool {
	for _, control := range controls {
		if control.Name == "callback" {
			return true
		}
	}
	return false
}

// dedupe keeps the watch list short. The same host can arrive twice: a marker
// URL that was re-fired and a receipt that recorded the same exchange.
func dedupe(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// fetch performs one exchange for the verification, through the same gate,
// budget and receipt trail every other request goes through.
func (f *FindingTool) fetch(ctx context.Context, url string, method string, body string) (recon.Result, []byte, error) {
	if err := recon.Gate(url); err != nil {
		return recon.Result{}, nil, err
	}
	if err := recon.Take(1); err != nil {
		return recon.Result{}, nil, err
	}
	return recon.ProbeCapture(ctx, recon.Request{URL: url, Method: method, Body: body}, 256*1024)
}

// findingControls picks the controls to run. The default set includes the
// secret control, which contradicts on a response with no credential in it,
// so a caller verifying something that is not a credential picks the ones it
// wants.
func findingControls(raw any) ([]verify.Control, error) {
	names := findingControlNames(raw)
	if len(names) == 0 {
		return verify.DefaultControls(), nil
	}
	all := map[string]verify.Control{}
	for _, control := range verify.AllControls() {
		all[control.Name] = control
	}
	picked := make([]verify.Control, 0, len(names))
	for _, name := range names {
		control, ok := all[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			return nil, fmt.Errorf("unknown control %q; the controls are %s", name, strings.Join(findingControlCatalogue(all), ", "))
		}
		picked = append(picked, control)
	}
	if len(picked) == 0 {
		return nil, fmt.Errorf("no control was selected")
	}
	return picked, nil
}

// findingControlNames reads the control names in both native and JSON-decoded
// shapes, in the order they were given.
func findingControlNames(value any) []string {
	var out []string
	switch typed := value.(type) {
	case []string:
		out = append(out, typed...)
	case []any:
		for _, entry := range typed {
			text, _ := entry.(string)
			out = append(out, text)
		}
	}
	return out
}

// findingControlCatalogue lists the control names for an error message.
//
// They are sorted, because an error that lists the controls in a different
// order on every call is an error nobody can compare against the last one.
func findingControlCatalogue(all map[string]verify.Control) []string {
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
