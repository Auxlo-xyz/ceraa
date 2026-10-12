package memory

import (
	"context"
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
//
// Gemini, served through its OpenAI-compatible surface at /v1beta/openai, so
// the existing chat-completions client drives it with no adapter. It replaced a
// Poolside laguna default after an identity probe: on the same prompt and the
// transcript, Poolside returned three different documents across runs at the
// same temperature with zero overlap between them, while Gemini held a correct
// document on every run and on both temperatures tested. Extraction runs off the
// hot path but its output is committed to long-lived memory, so a provider that
// samples a different answer each time is the wrong one to build on.
const (
	DefaultExtractBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
	DefaultExtractModel   = "gemini-3.5-flash-lite"
)

// extractorPrompt is the whole system instruction. It is tight on purpose:
// the model is asked for statements about durable preferences, not for
// anything a session file already has.
const extractorPrompt = `Extract durable claims from the conversation and existing memory summary about how the user wants work done, plus standing facts that will still hold later.

Keep: standing rules, conventions, tool choices, stated role and stack, likes and dislikes, goals, inferred traits and mistakes to avoid (with the fix).

Skip: task state, files, one-off requests.

Output one claim per line: ` + "`type | applies | kind | confidence | statement`" + `

- type: instruction, preference, profile, goal, decision, constraint, error, or context
- applies: a path glob, or "all"
- kind: "add", or "contradict" if the user reversed an existing claim
- confidence: "high" if explicit, "medium" if clearly implied and repeated; skip weaker ones
- statement: one concrete sentence, no "|" characters

Output nothing else. If nothing qualifies, output []`

// Extracted is one candidate the model produced.
type Extracted struct {
	Statement string `json:"statement"`
	// Type is what kind of claim this is, and it decides the lifetime. Empty is
	// allowed and means the classifier decides from the statement's wording, which
	// is also what an older build of the extractor leaves behind.
	Type       string `json:"type"`
	Applies    string `json:"applies"`
	Kind       string `json:"kind"`
	Confidence string `json:"confidence"`
}

// MemoryTypeFromName validates a type the model named.
//
// An unrecognised value returns "" rather than a guess, and the caller falls back
// to the classifier. That is the difference between a model that invented a type
// and a memory quietly assigned the wrong lifetime: the first is discarded, the
// second is not detected at all.
//
// The aliases exist because a model asked for one word will reach for the nearest
// one it knows, and "instruction" arriving as "rule" is not a signal worth losing.
func MemoryTypeFromName(name string) MemoryType {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "instruction", "rule", "directive":
		return Instruction
	case "preference":
		return Preference
	case "profile", "identity", "about":
		return Profile
	case "goal":
		return Goal
	case "decision", "choice":
		return Decision
	case "constraint", "limit", "boundary":
		return Constraint
	case "request", "task":
		return Request
	case "context", "project":
		return Context
	case "error", "mistake", "lesson":
		return Error
	case "observation":
		return Observation
	default:
		return ""
	}
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
// decides what gets written. This separation keeps the log's approval model
// honest, because the model cannot write memory without going through the
// writer the caller chose.
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

// parseExtracted turns the model's text into statements.
//
// The line format is `type | applies | kind | confidence | statement`, with
// the statement last so a statement containing a pipe would still be readable
// in full rather than being split at the wrong field. The prompt forbids them
// but a parser that trusts that loses the claim.
//
// A line it cannot read is dropped and the rest are kept. That is the reason
// for this format over JSON: a small model that emits one bad line loses that
// line instead of the whole reply, because brace-matching a truncated array
// fails closed on everything. Nothing here returns an error for bad content,
// because a reply that is entirely unusable and a reply with three good claims
// are both just a fact about the model, and neither is worth failing a
// compaction over.
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
	// The empty answer, spelled the way the prompt spells it, and the JSON one
	// an older build of the extractor was asked for. Both mean nothing claimed.
	if text == "" || text == "[]" || strings.TrimLeft(text, "` \t") == "[]" {
		return nil, nil
	}

	cleaned := make([]Extracted, 0, 8)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "|"))
		if line == "" || line == "[]" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "|", 5)
		if len(parts) < 5 {
			continue
		}
		item := Extracted{
			Type:       strings.TrimSpace(parts[0]),
			Applies:    strings.TrimSpace(parts[1]),
			Kind:       strings.TrimSpace(parts[2]),
			Confidence: strings.TrimSpace(parts[3]),
			Statement:  strings.TrimSpace(parts[4]),
		}
		// "all" is how the prompt says everywhere, and "" is what the rest of
		// this package reads as everywhere. Translated here so nothing
		// downstream has to know which prompt is installed.
		if strings.EqualFold(item.Applies, "all") {
			item.Applies = ""
		}
		// A statement is the whole point of a line, so a line without one is not
		// a claim. Confidence too: a claim of unknown strength cannot be given a
		// lifetime, and guessing one is how a maybe becomes a standing fact.
		if item.Statement == "" || item.Confidence == "" {
			continue
		}
		cleaned = append(cleaned, item)
	}
	return cleaned, nil
}

// personalMemoryLogLookback is how far back the log is checked before appending
// extracted personal facts. It bounds the duplicate check at compaction, which
// is rare, without letting one unchecked older copy turn into a new copy on
// every fold.
const personalMemoryLogLookback = 200

// personalMemoryMaxStatements bounds one compaction's user-memory write. The log
// already receives the checkpoint summary itself, so this writer carries only
// what the log cannot already say in compressed form.
const personalMemoryMaxStatements = 8

// personalMemoryMaxEntryRunes bounds one compaction's user-memory write. An
// unbounded profile would become the per-turn bulk this design removed.
const personalMemoryMaxEntryRunes = 1600

// personalMemoryKinds are the extractor types that belong in the append-only
// log as durable facts about the person rather than the task.
func personalMemoryKinds() map[MemoryType]bool {
	return map[MemoryType]bool{
		Profile:    true,
		Preference: true,
		Goal:       true,
		Constraint: true,
	}
}

// personalMemoryLabel names the kind in the rendered note.
func personalMemoryLabel(kind MemoryType) string {
	switch kind {
	case Profile:
		return "Profile"
	case Preference:
		return "Preference"
	case Goal:
		return "Goal"
	case Constraint:
		return "Constraint"
	default:
		return "Memory"
	}
}

// AppendPersonalMemories writes newly extracted durable user facts to the
// append-only log, without any extra model call.
//
// It uses the statements Extract already returned, so the one model call pays
// for both the summary the compactor keeps and the durable facts the log
// keeps. Only high- and medium-confidence profile, preference, goal and constraint claims are
// carried: task state, requests, decisions, errors and context belong in the
// turn ledger or the checkpoint summary rather than in a durable picture of the
// person. A claim already present in the recent log is not repeated, because the
// log has no rewrite operation and a duplicate would otherwise be permanent.
func AppendPersonalMemories(log *MemoryFile, sessionID string, statements []Extracted) int {
	if log == nil || len(statements) == 0 {
		return 0
	}
	seen := make(map[string]struct{})
	for _, entry := range log.Tail(personalMemoryLogLookback) {
		body := entry
		if line := strings.IndexByte(entry, '\n'); line >= 0 {
			body = entry[line+1:]
		}
		for _, line := range strings.Split(body, "\n") {
			normalised := strings.Join(strings.Fields(line), " ")
			if normalised != "" {
				seen[normalised] = struct{}{}
			}
		}
	}

	kinds := personalMemoryKinds()
	lines := make([]string, 0, personalMemoryMaxStatements)
	for _, statement := range statements {
		if len(lines) >= personalMemoryMaxStatements {
			break
		}
		text := strings.Join(strings.Fields(statement.Statement), " ")
		if text == "" || VeracityForConfidence(statement.Confidence) == "" {
			continue
		}
		kind := MemoryTypeFromName(statement.Type)
		if !kinds[kind] {
			continue
		}
		line := personalMemoryLabel(kind) + ": " + text
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return 0
	}
	entry := truncatePersonalMemory("Durable user memory:\n" + strings.Join(lines, "\n"))
	if err := log.Append(entry, sessionID); err != nil {
		logger.Warn("memory log personal append: " + err.Error())
		return 0
	}
	return len(lines)
}

// truncatePersonalMemory bounds one user-memory write without splitting a rune.
func truncatePersonalMemory(text string) string {
	runes := []rune(text)
	if len(runes) <= personalMemoryMaxEntryRunes {
		return text
	}
	return string(runes[:personalMemoryMaxEntryRunes])
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

// MakeLearner ties extraction to the append-only log. It returns a
// compaction.Learn hook, or nil when no extraction credential is configured,
// which makes the "zero-cost model" opt-in rather than a hard requirement. An
// empty config does not disable compaction; it just skips the memory-mining
// step that follows it.
func MakeLearner(main llm.ProviderConfig, log *MemoryFile) (LearnFunc, bool) {
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
		// The same extraction already paid for the model call, so the log gets
		// the personal subset without a second one. Durable facts about the user
		// belong in the log the sidekick actually reads; task-scoped claims stay
		// in the turn ledger and the checkpoint summary.
		if added := AppendPersonalMemories(log, sessionID, extracted); added > 0 {
			logger.Info(fmt.Sprintf("memory extractor: logged=%d", added))
		}
	}, true
}
