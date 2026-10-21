package tool

// recon_finding.go is the finding record wearing a tool face.
//
// The store it writes is the harness/findings package; this file is only the
// shape the model talks to. Two things are decided here rather than there:
//
//   - what a mode does. add records, update changes the fields the model
//     named and leaves the rest alone, list and coverage read, delete
//     removes. An update that defaulted the severity would overwrite the
//     severity the record already carried, which is how a record quietly
//     stops describing what it describes.
//   - what the chat sees. One finding per block, and the reproduction
//     printed verbatim, because the PoC is the thing a reader copies and a
//     copy that needs un-escaping is a copy that gets pasted wrong.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"ceraa/harness/findings"
	"ceraa/harness/operation"
	"ceraa/internal/paths"
)

// FindingTool records what a recon run found, ruled out and left open.
type FindingTool struct{}

// The executor resolves a tool by its operation type and calls it through the
// context runner, so the two have to agree with the name.
var _ operation.ContextRunner = (*FindingTool)(nil)

func (f *FindingTool) Name() string { return "finding" }

func (f *FindingTool) Description() string {
	return "Record what a recon run found, what it ruled out and what it left open, and render the record as a disclosure report. " +
		"Modes: add (record a finding), update (change the fields it names), list, delete, coverage. " +
		"Every finding carries a closure state: confirmed (proved), ruled_out (proved not to be a problem, with the reason and the control that stops it) or open_proof_gap (suspected, with the step that would close it). " +
		"A disclosure that leaves out the gaps and the ruled out items cannot tell the reader what was checked. " +
		"Fields: a title, a severity, the reproduction as a curl line or a script, the evidence that was observed, and the counterevidence that argues against it. " +
		"The record is one file per finding under the data directory."
}

func (f *FindingTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{
				"type":        "string",
				"enum":        []string{"add", "update", "list", "delete", "coverage"},
				"description": "add (default) records a finding, update changes the fields it names, list shows the record, delete removes one, coverage counts it",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "The finding's id. Generated from the title when absent, so a second call about the same finding lands on the same record",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "What the finding is, in one line. Required for add and for update",
			},
			"severity": map[string]any{
				"type":        "string",
				"enum":        []string{"info", "low", "medium", "high", "critical"},
				"description": "How bad it is. Default medium for a new finding; an update that does not name one keeps the severity the record carries",
			},
			"status": map[string]any{
				"type":        "string",
				"enum":        []string{"confirmed", "ruled_out", "open_proof_gap"},
				"description": "The closure state. confirmed means proved, ruled_out means proved not to be a problem, open_proof_gap means suspected with one step left to close it. Default confirmed for a new finding",
			},
			"url":         map[string]any{"type": "string", "description": "The URL the finding is about"},
			"endpoint":    map[string]any{"type": "string", "description": "The endpoint, as a method and a path"},
			"description": map[string]any{"type": "string", "description": "What the finding is and why it matters"},
			"poc":         map[string]any{"type": "string", "description": "The reproduction: a curl line or a script, carried verbatim into the report"},
			"evidence":    map[string]any{"type": "string", "description": "What was observed, as it was seen"},
			"counterevidence": map[string]any{
				"type":        "string",
				"description": "What argues against the finding. A confirmed finding without it reads as a finding nobody argued with",
			},
			"remediation": map[string]any{"type": "string", "description": "What would fix it"},
			"cvss": map[string]any{
				"type":        "string",
				"description": "A CVSS v3.1 vector, or the eight metrics on their own, as AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H. The score is computed from it and reported back",
			},
			"tags": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"maxItems":    12,
				"description": "Short labels for grouping the finding in the report",
			},
		},
		// Nothing is required by the schema: the mode decides. A title is
		// required to add or update a finding and an id to delete one, and
		// listing the record needs neither.
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
	case "add", "update":
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
		} else if mode == "add" {
			out["severity"] = findings.SeverityMedium
		}
		if status := strings.TrimSpace(findingString(args["status"])); status != "" {
			parsed, err := findings.ParseStatus(status)
			if err != nil {
				return operation.Spec{}, err
			}
			out["status"] = parsed
		} else if mode == "add" {
			out["status"] = findings.StatusConfirmed
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

	case "delete":
		id := strings.TrimSpace(findingString(args["id"]))
		if id == "" {
			return operation.Spec{}, fmt.Errorf("id is required")
		}
		out["id"] = id
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
	store, err := findings.Open(findingsDir())
	if err != nil {
		return "", err
	}
	mode, _ := spec.Args["mode"].(string)

	switch mode {
	case "add":
		stored, err := store.Put(findingFromArgs(spec.Args, findings.Finding{}))
		if err != nil {
			return "", err
		}
		return renderFindingBlock("recorded", stored), nil

	case "update":
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

	case "list":
		return renderFindingList(store)

	case "delete":
		id, _ := spec.Args["id"].(string)
		if err := findings.ValidateID(id); err != nil {
			return "", err
		}
		if err := store.Delete(id); err != nil {
			return "", err
		}
		return fmt.Sprintf("deleted finding %s\n", id), nil

	case "coverage":
		return renderFindingCoverage(store)
	}
	return "", fmt.Errorf("mode %q is not one of add, update, list, delete, coverage", mode)
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
		return "add", nil
	}
	switch mode {
	case "add", "update", "list", "delete", "coverage":
		return mode, nil
	}
	return "", fmt.Errorf("mode %q is not one of add, update, list, delete, coverage", mode)
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
