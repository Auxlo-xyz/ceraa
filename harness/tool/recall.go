package tool

import (
	"fmt"
	"strings"

	"ceraa/harness/compaction"
	"ceraa/harness/operation"
)

// RecallArchiveTool searches the compaction archive: the pre-compaction
// history a summary replaced. Summaries are lossy and this is the way back.
type RecallArchiveTool struct{}

func (t *RecallArchiveTool) Name() string { return "recall_archive" }
func (t *RecallArchiveTool) Description() string {
	return "Search archived pre-compaction history, or open one archived entry whole."
}
func (t *RecallArchiveTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"regex":   map[string]any{"type": "string", "description": "Case-insensitive pattern to search summaries and discarded messages."},
			"entry":   map[string]any{"type": "integer", "minimum": 0, "description": "Open one archived entry whole instead of searching. Search results name the number."},
			"session": map[string]any{"type": "string", "description": "Another session's archive. Defaults to this run's session."},
		},
	}
}
func (t *RecallArchiveTool) Translate(args map[string]any) (operation.Spec, error) {
	regex, _ := args["regex"].(string)
	var entry *int
	if raw, ok := args["entry"]; ok && raw != nil {
		var n int
		switch value := raw.(type) {
		case int:
			n = value
		case int64:
			n = int(value)
		case float64:
			n = int(value)
		default:
			return operation.Spec{}, fmt.Errorf("entry must be a number")
		}
		if n < 0 {
			return operation.Spec{}, fmt.Errorf("entry must be zero or more")
		}
		entry = &n
	}
	session, _ := args["session"].(string)
	if strings.TrimSpace(regex) == "" && entry == nil {
		return operation.Spec{}, fmt.Errorf("pass regex to search or entry to open")
	}
	specArgs := map[string]any{"session": session}
	if entry != nil {
		specArgs["entry"] = *entry
	} else {
		specArgs["regex"] = regex
	}
	return operation.Spec{Type: "recall_archive", Args: specArgs}, nil
}
func (t *RecallArchiveTool) TranslateResult(op operation.Operation) string { return op.Output }
func (t *RecallArchiveTool) Run(spec operation.Spec) (string, error) {
	dir, _ := spec.Args["archive_dir"].(string)
	session, _ := spec.Args["session"].(string)
	if strings.TrimSpace(session) == "" {
		session, _ = spec.Args["session_id"].(string)
	}
	if strings.TrimSpace(session) == "" {
		session = "default"
	}
	if raw, ok := spec.Args["entry"]; ok && raw != nil {
		var entry int
		switch value := raw.(type) {
		case int:
			entry = value
		case int64:
			entry = int(value)
		case float64:
			entry = int(value)
		default:
			return "", fmt.Errorf("entry must be a number")
		}
		return compaction.OpenArchive(dir, session, entry)
	}
	regex, _ := spec.Args["regex"].(string)
	if strings.TrimSpace(regex) == "" {
		return "", fmt.Errorf("pass regex to search or entry to open")
	}
	return compaction.SearchArchive(dir, session, regex)
}
