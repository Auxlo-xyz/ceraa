package tool

import (
	"encoding/json"
	"fmt"
	"strings"

	"ceraa/harness/operation"
	"ceraa/harness/question"
)

// QuestionTool asks user a question with buttons, maps to soraacode tool/question.ts + question-manager
type QuestionTool struct{}

func (q *QuestionTool) Name() string { return "question" }
func (q *QuestionTool) Description() string {
	return "Ask the user a question, either with options that become buttons in Telegram, or " +
		"free-form so they can answer in their own words."
}
func (q *QuestionTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{"type": "string", "description": "Question to ask"},
			"options": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Answer options (2-4) offered as buttons. Omit, or pass free_form, when the user should be able to answer in their own words. The user can always type their own answer instead via the Type-an-answer button, so options are suggestions, never the only allowed answers.",
			},
			"free_form": map[string]any{
				"type":        "boolean",
				"description": "Let the user type any answer instead of choosing one of the options. The options, if any, are then offered as suggestions.",
			},
			"header": map[string]any{"type": "string", "description": "Header for question"},
		},
		"required": []string{"question"},
	}
}
func (q *QuestionTool) Translate(args map[string]any) (operation.Spec, error) {
	ques, ok := args["question"].(string)
	if !ok || strings.TrimSpace(ques) == "" {
		return operation.Spec{}, fmt.Errorf("question is required")
	}
	spec := operation.Spec{Type: "question", Args: map[string]any{"question": ques}}
	if v, ok := args["options"]; ok {
		spec.Args["options"] = v
	}
	if v, ok := args["header"]; ok {
		spec.Args["header"] = v
	}
	if v, ok := args["free_form"]; ok {
		if enabled, isBool := v.(bool); isBool && enabled {
			spec.Args["free_form"] = true
		}
	}
	// Free-form is the default when no options are given, because a question with
	// only Yes and No is rarely what was meant.
	if _, hasOptions := spec.Args["options"]; !hasOptions {
		spec.Args["free_form"] = true
	}
	// Pass sessionID if available in args (set by agent-bridge)
	if v, ok := args["session_id"]; ok {
		spec.Args["session_id"] = v
	}
	return spec, nil
}
func (q *QuestionTool) TranslateResult(op operation.Operation) string { return op.Output }

// QuestionRunner waits for Telegram answer
type QuestionRunner struct {
	SessionID string
}

func (r *QuestionRunner) Run(spec operation.Spec) (string, error) {
	ques, _ := spec.Args["question"].(string)
	var opts []string
	if v, ok := spec.Args["options"]; ok {
		switch arr := v.(type) {
		case []string:
			opts = arr
		case []any:
			for _, o := range arr {
				if s, ok := o.(string); ok {
					opts = append(opts, s)
				}
			}
		}
	}
	header, _ := spec.Args["header"].(string)
	sessionID, _ := spec.Args["session_id"].(string)
	if sessionID == "" {
		sessionID = r.SessionID
	}
	if sessionID == "" {
		sessionID = "default"
	}
	requestID := fmt.Sprintf("q-%d", len(ques)) // simple, operation ID will be better
	// Use operation ID as requestID if available
	if id, ok := spec.Args["request_id"].(string); ok && id != "" {
		requestID = id
	}
	// Publish via question manager and wait
	// The bridge will show buttons and callback will answer
	// For harness-only use without bridge, just return question
	if len(opts) == 0 {
		opts = []string{"Yes", "No"}
	}
	// Build display
	var b strings.Builder
	if header != "" {
		b.WriteString(header + "\n\n")
	}
	b.WriteString(ques + "\n")
	for i, o := range opts {
		b.WriteString(fmt.Sprintf("%d. %s\n", i+1, o))
	}
	// Wait for answer via manager. The timeout is the manager's default: a
	// question holds its run until the human answers, and nothing above it
	// expires first.
	answer, err := question.Default.Create(sessionID, requestID, ques, opts, question.DefaultTimeout)
	if err != nil {
		// Fallback: return display for non-interactive mode
		data, _ := json.Marshal(map[string]any{"question": ques, "options": opts, "display": b.String()})
		return string(data), nil
	}
	return answer, nil
}
