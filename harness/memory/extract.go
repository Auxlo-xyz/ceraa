package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
	"ceraa/internal/envs"
	"ceraa/internal/logger"
)

// DefaultExtractBaseURL and DefaultExtractModel point the extractor at the
// provider's endpoint and model by default. They are constants rather than
// guesses so the live path is auditable and the config is explicit.
const (
	DefaultExtractBaseURL = "https://inference.poolside.ai/v1"
	DefaultExtractModel   = "poolside/laguna-s-2.1"
)

// extractorPrompt is the whole system instruction. It is tight on purpose:
// the model is asked for statements about durable preferences, not for
// anything a session file already has.
const extractorPrompt = `You are a preference extractor for a coding agent. Read the conversation and the checkpoint summary, and extract durable facts about HOW the user wants work done: formatting rules, tool preferences, review discipline, conventions, and explicit reversals of previous preferences.

Do not extract task state, files, temporary details, or anything that does not generalize across sessions. Return ONLY a JSON array of objects, no prose before or after, each with exactly these string fields:
- "statement": a single concrete sentence about the user's preference. Name things that are stable.
- "applies": a path prefix or glob this preference is about, or "" if it applies everywhere.
- "kind": "add" for a new preference, "contradict" if the statement reverses an existing preference.
- "confidence": "high" only if the conversation gives a clear, explicit signal; "medium" for inferred; "low" for weak.

If nothing in the conversation reveals a durable preference, return an empty array: [].`

// Extracted is one candidate the model produced.
type Extracted struct {
	Statement  string `json:"statement"`
	Applies    string `json:"applies"`
	Kind       string `json:"kind"`
	Confidence string `json:"confidence"`
}

// ExtractConfigFromENV reads the extraction provider override from the
// environment and falls for the API key onto the main provider only when it is
// the same endpoint, otherwise the key is empty and extraction is disabled
// rather than attempted with a credential from the wrong service.
func ExtractConfigFromENV(main llm.ProviderConfig) (llm.ProviderConfig, bool) {
	c := llm.ProviderConfig{
		BaseURL: envs.Get("EXTRACT_URL"),
		Model:   envs.Get("EXTRACT_MODEL"),
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultExtractBaseURL
	}
	if c.Model == "" {
		c.Model = DefaultExtractModel
	}
	c.APIKey = envs.Get("EXTRACT_KEY")
	if c.APIKey == "" && strings.EqualFold(c.BaseURL, main.BaseURL) {
		c.APIKey = main.APIKey
	}
	if c.APIKey == "" {
		return c, false
	}
	return c, true
}

// Extract runs extraction over the discarded head of a session and a summary,
// returning the candidate statements.
//
// It performs exactly one LLM call. No tool calls, no persistence: the caller
// decides what gets stored. This separation keeps the store's approval model
// honest, because the model cannot write memory without going through Commit.
func Extract(ctx context.Context, cfg llm.ProviderConfig, discarded []sessionstore.Message, summary string) ([]Extracted, error) {
	if len(discarded) == 0 {
		return nil, nil
	}
	var head strings.Builder
	for _, m := range discarded {
		head.WriteString(strings.ToUpper(string(m.Role)))
		head.WriteString(": ")
		head.WriteString(m.Content)
		head.WriteString("\n\n")
	}
	payload := "CHECKPOINT SUMMARY:\n" + strings.TrimSpace(summary) + "\n\nDISCARDED CONVERSATION:\n" + head.String()
	client := llm.New(cfg)
	req := llm.StreamRequest{
		Messages: []llm.Item{
			{Kind: "text", Role: llm.RoleSystem, Text: extractorPrompt},
			{Kind: "text", Role: llm.RoleUser, Text: payload},
		},
		MaxTokens: 2048,
	}
	var out strings.Builder
	err := client.Stream(ctx, req, func(delta llm.ProviderResponse) {
		if delta.Content != "" {
			out.WriteString(delta.Content)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("extraction LLM call: %w", err)
	}
	return parseExtracted(out.String())
}

// parseExtracted turns the model's text into statements, tolerating a code
// fence the model was told not to add.
func parseExtracted(text string) ([]Extracted, error) {
	text = strings.TrimSpace(text)
	// Strip a markdown fence if one was emitted anyway.
	if strings.HasPrefix(text, "```") {
		if idx := strings.Index(text, "\n"); idx >= 0 {
			text = text[idx+1:]
		}
		if strings.HasSuffix(text, "```") {
			text = strings.TrimSuffix(text, "```")
		}
		text = strings.TrimSpace(text)
	}
	if text == "" || text == "[]" || strings.TrimLeft(text, "` \t") == "[]" {
		return nil, nil
	}
	var out []Extracted
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("parse extraction output as JSON array: %w\nfirst 400 bytes: %s", err, truncateUTF4(text, 400))
	}
	cleaned := make([]Extracted, 0, len(out))
	for _, item := range out {
		item.Statement = strings.TrimSpace(item.Statement)
		item.Applies = strings.TrimSpace(item.Applies)
		item.Confidence = strings.TrimSpace(item.Confidence)
		if item.Statement == "" || item.Confidence == "" {
			continue
		}
		cleaned = append(cleaned, item)
	}
	return cleaned, nil
}

// Commit writes the high-confidence statements, and treats "contradict" as a
// candidate for removal that is reported rather than acted on automatically.
//
// We deliberately do not auto-delete: removals are the one mutation that cannot
// be un-done by adding the statement back, so a wrong "contradict" would
// silently erase a correct preference. The model's job is to nominate; the
// store's explicit contradict-by-id entry point stays the only thing that
// deletes.
func Commit(store *Store, projectID, sessionID string, statements []Extracted) (added int, contradicted int, err error) {
	for _, s := range statements {
		veracity := VeracityForConfidence(s.Confidence)
		if veracity == "" {
			// An unrecognised confidence is dropped rather than guessed at. Storing
			// it as Unknown would launder a value this build does not understand
			// into the middle of the table, which is worse than losing it.
			continue
		}
		if strings.EqualFold(s.Kind, "contradict") {
			contradicted++
			continue
		}
		if strings.EqualFold(s.Kind, "add") || strings.EqualFold(s.Kind, "confirm") {
			if _, err := store.AddScoped(projectID, sessionID, s.Statement, s.Applies, "learned at compaction", veracity); err != nil {
				return added, contradicted, err
			}
			added++
		}
	}
	return added, contradicted, nil
}

// VeracityForConfidence maps the extractor's own confidence onto a provenance.
//
// The extractor's "high" means the conversation gave a clear explicit signal,
// which is what Stated means here, so a high-confidence extraction is trusted as
// though the user had said it. Medium is the extractor's inference and lands on
// Inferred, which is the common case and is meant to be common.
//
// A "low" confidence has no equivalent, and that is deliberate. A weak guess is
// not a lesser fact, it is a guess; the store has no way to keep it out of a
// prompt except by weighting it, and a guess that was never stored is a guess the
// agent will confidently re-derive and present as knowledge. It returns "" so
// Commit skips it. Storing it at all, let alone letting it be re-asserted, was the
// behaviour this replaces: the old filter keyed on the string "high" and threw the
// rest away, so a medium inference could never be learned however often it was
// re-asserted.
func VeracityForConfidence(confidence string) Veracity {
	switch strings.ToLower(strings.TrimSpace(confidence)) {
	case "high", "explicit", "stated":
		return Stated
	case "medium", "inferred", "moderate":
		return Inferred
	default:
		return ""
	}
}

// LearnFunc matches the compaction hook's signature so the extractor can be
// attached to a compactor directly.
type LearnFunc func(context.Context, string, []sessionstore.Message, string)

// MakeLearner ties extraction to a memory store and a project.
// It returns a compaction.Learn hook, or nil when no extraction credential is
// configured, which makes the "zero-cost model" opt-in rather than a hard
// requirement. An empty config does not disable compaction; it just skips the
// memory-mining step that follows it.
func MakeLearner(main llm.ProviderConfig, store *Store, projectID string) (LearnFunc, bool) {
	cfg, enabled := ExtractConfigFromENV(main)
	if !enabled {
		return nil, false
	}
	return func(ctx context.Context, sessionID string, discarded []sessionstore.Message, summary string) {
		extracted, err := Extract(ctx, cfg, discarded, summary)
		if err != nil {
			logger.Warn("memory extractor: " + err.Error())
			return
		}
		added, contradicted, err := Commit(store, projectID, sessionID, extracted)
		if err != nil {
			logger.Warn("memory extractor commit: " + err.Error())
			return
		}
		if added > 0 || contradicted > 0 {
			logger.Info(fmt.Sprintf("memory extractor: added=%d contradicted=%d", added, contradicted))
		}
	}, true
}

// truncateUTF4 is a bounded version of an error-message snippet so a giant
// model reply does not blow up the log.
func truncateUTF4(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
