package internal

import (
	"context"
	"encoding/json"
	"strings"

	"ceraa/harness/instruction"
	"ceraa/harness/llm"
	"ceraa/internal/envs"
)

const scopePrompt = `You assign scopes to project rules.

You are given rules that declare no scope, and a list of paths that actually
appear in this repository. For each rule, choose the ONE path from the list that
the rule is about, or answer nothing for that rule.

Answer only with a JSON array. Each element is {"id":"...","applies":"..."} where
id is the rule's id exactly as given, and applies is a path copied from the list.
Do not invent a path. Do not explain.`

// ProposeScopes asks a small model to scope rules that named nothing the
// filesystem could confirm.
//
// It exists because tagging by hand does not survive a growing file: a rule added
// today with no scope compiles unscoped, an unscoped rule is delivered once per
// run and never again, and nothing says so.
//
// The safety property is that the model may only choose from the candidate list.
// A proposer allowed to invent paths would need the caller to guess at every
// suggestion, and the answer to a suggestion that might be a hallucination is to
// refuse it - so a proposer constrained to the list either answers something
// real or answers nothing. Confirmation still happens, because the list contains
// tokens that turned out not to be paths at all (Go imports, module paths, shell
// variables), but the model cannot widen it.
func ProposeScopes(main llm.ProviderConfig) (instruction.Proposer, bool) {
	cfg, enabled := scopeConfigFromENV(main)
	if !enabled {
		return nil, false
	}
	client := llm.New(cfg)
	return func(ctx context.Context, req instruction.ProposeRequest) ([]instruction.ScopeProposal, error) {
		if len(req.Rules) == 0 || len(req.Candidates) == 0 {
			return nil, nil
		}
		var body strings.Builder
		body.WriteString("PATHS:\n")
		for _, candidate := range req.Candidates {
			body.WriteString("- ")
			body.WriteString(candidate)
			body.WriteString("\n")
		}
		body.WriteString("\nRULES:\n")
		for _, rule := range req.Rules {
			body.WriteString("id=")
			body.WriteString(string(rule.ID))
			body.WriteString(": ")
			body.WriteString(rule.Text)
			body.WriteString("\n")
		}

		var out strings.Builder
		if err := client.Stream(ctx, llm.StreamRequest{
			Messages: []llm.Item{
				{Kind: "text", Role: llm.RoleSystem, Text: scopePrompt},
				{Kind: "text", Role: llm.RoleUser, Text: body.String()},
			},
			MaxTokens: 2048,
		}, func(delta llm.ProviderResponse) {
			out.WriteString(delta.Content)
		}); err != nil {
			return nil, err
		}
		return parseScopes(out.String())
	}, true
}

// parsedScope is one element of the array the model is asked for.
type parsedScope struct {
	RuleID  string `json:"id"`
	Applies string `json:"applies"`
}

// parseScopes reads the model's answer and returns only well-formed elements.
//
// An element with no id or no applies is dropped rather than defaulted, because
// either field missing means the pair cannot be tied to a rule and applying it
// would silently mis-scope an unrelated one. The confirmation step in TagScopes
// then decides what survives, so a dropped element is a rule left alone rather
// than a rule withheld.
func parseScopes(text string) ([]instruction.ScopeProposal, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, nil
	}
	start := strings.Index(trimmed, "[")
	end := strings.LastIndex(trimmed, "]")
	if start < 0 || end < start {
		return nil, nil
	}
	var raw []parsedScope
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &raw); err != nil {
		return nil, nil
	}
	var out []instruction.ScopeProposal
	for _, item := range raw {
		id := strings.TrimSpace(item.RuleID)
		applies := strings.TrimSpace(item.Applies)
		if id == "" || applies == "" {
			continue
		}
		out = append(out, instruction.ScopeProposal{
			RuleID:  instruction.ID(id),
			Applies: applies,
			Why:     "",
		})
	}
	return out, nil
}

// scopeConfigFromENV reads the proposer's own credentials, sharing the memory
// extractor's convention.
//
// Separate EXTRACT_* spelling rather than reusing it, because the two calls have
// different shapes and a key that has to work for both is a key scoped to neither.
// Proposer is the name the AGENTS.md/MODEL docs use for a rule scoping it, so the
// prefix is that as well.
func scopeConfigFromENV(main llm.ProviderConfig) (llm.ProviderConfig, bool) {
	cfg := llm.ProviderConfig{
		BaseURL: envs.Get("PROPOSER_URL"),
		Model:   envs.Get("PROPOSER_MODEL"),
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = main.BaseURL
	}
	if cfg.Model == "" {
		cfg.Model = main.Model
	}
	cfg.APIKey = envs.Get("PROPOSER_KEY")
	if cfg.APIKey == "" && strings.EqualFold(cfg.BaseURL, main.BaseURL) {
		cfg.APIKey = main.APIKey
	}
	if cfg.APIKey == "" {
		return cfg, false
	}
	return cfg, true
}
