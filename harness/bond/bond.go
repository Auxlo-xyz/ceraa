// Package bond maintains a user-level profile of the person Ceraa works for.
//
// It differs from project memory in scope and in ownership. Memory is
// per-project and the model mines it. The bond is one document about one
// person across every project, and the design keeps the model advisory: it
// proposes, a human commits. The rendered law in memory/bond.go says so on
// every turn.
//
// The rolling window is the part that took probing to settle. Two documents
// are kept. A fresh compaction checkpoint is one context; the last two
// documents are the ones reconciled with it. The first round has no history
// and runs on its context alone, which is the whole "new user" path.
//
// Triggered off compaction, not off turns: a checkpoint is the only place the
// transcript has been deliberately condensed, so it is the only place a
// context worth summarizing exists, and it costs nothing per turn.
package bond

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/sessionstore"
	"ceraa/internal/logger"
)

// ExtractorPrompt is the owner's prompt, used word for word.
//
// Two properties of it are load-bearing and were established by probing, not
// by taste. Its trailing "Write the BOND document now, filling every field you
// can support" is what makes the model produce a document at all: with that
// sentence removed the same prompt returned "Understood." on five runs of five,
// because every imperative had been replaced by a description of what the
// document is. And its Updating section is what lets a merge pass reconcile
// two independent readings instead of overwriting one with the other.
//
// Do not trim the imperatives to make the prompt read better. The brevity
// rules that were cut in earlier drafts were doubling as activation cues.
const ExtractorPrompt = `# BOND

Analyze the conversation and maintain a compact behavioral and emotional model of the user. Your goal is to preserve the precise amount of user-specific context that will improve how the agent works with them.

## Extraction Rules

Only record information supported by the conversation.

Prefer repeated or strongly expressed signals over one-off statements.
Infer behavior and emotional patterns.
Do not invent facts, diagnose mental states, or speculate about personality.
Prioritize information that will improve how the agent should respond.
Remove redundant or obvious information.
Current emotional state may change; durable patterns should not be overwritten by temporary states.

## User

Name: [preferred name explicitly given or used]
Role: [what the user does/builds]
Goals: [important goals currently pursued]
Stack: [technologies/tools repeatedly used or explicitly preferred]

## Mind

Mood: [current emotional state expressed or strongly indicated]
Energy: [low | medium | high, based on engagement/pace]
Pressure: [low | medium | high, based on urgency/stakes]
Confidence: [low | medium | high, based on certainty/self-assurance]

## Patterns

Motivates: [things that visibly increase engagement]
Frustrates: [things that repeatedly cause irritation/friction]
Excites: [topics/results that produce strong positive engagement]
Calms: [things that reduce frustration or uncertainty]

## Interaction

Wants: [how the user wants the agent to behave]
Hates: [behaviors/styles the user strongly rejects]
Needs when stuck: [most helpful agent response when blocked]
Needs when winning: [how to behave when progress is going well]
Posture: [tool, collaborator, or somewhere between]

## Rules

ALWAYS: [explicit or repeatedly demonstrated requirements]
NEVER: [explicit prohibitions]
PREFER: [consistent preferences]
AVOID: [consistent dislikes]

## Context

Focus: [what matters most right now]
Projects: [active projects relevant to future conversations]
Learned: [durable insights about the user that improve future interactions]

## Emotional Interpretation

Separate temporary state from durable traits.

Capture emotion when it is:
- explicitly stated
- strongly expressed through language
- repeatedly demonstrated
- directly relevant to how the agent should respond

Do not treat:
- short replies
- profanity
- enthusiasm about a single result
- frustration with a single bug
as permanent personality traits.

When emotion is detected, capture the actionable signal entirely rather than merely labeling it.

Prefer:

Mood: frustrated | Trigger: repeated deployment failures | Response: solve directly

Over:

Mood: frustrated

## Updating

If an existing BOND is provided:

- Preserve still-valid durable information.
- Update outdated information.
- Resolve contradictions using stronger or more recent evidence.
- Do not accumulate duplicates.
- Remove information that is clearly no longer true.
- Treat current Mind values as replaceable state, not permanent memory.
- Add genuinely new durable insights to Learned.

## Compression

Optimize for information density.
Merge related observations.
Do not store conversation history.
Do not duplicate information across sections.

Target: the smallest but context rich BOND that allows a future agent to understand the user significantly better than without it.

Write the BOND document now, filling every field you can support.`

// mergeInstruction frames the second pass. It is the reconciler that decided
// what to do with a fresh reading that contradicted an older one, which is the
// case a single pass cannot settle: it can hedge the fresh fact or drop the
// old one, and both lose information.
const mergeInstruction = `Reconcile the documents above into one BOND. Prefer the fresh extraction where they disagree about current state, keep durable information that only the earlier documents have, and drop anything no longer supported by any of them.`

// running serialises extractions. Without it two compactions in quick
// succession both read the same window and the second commit silently
// discards the first, which looks like the bond losing a round.
var running sync.Mutex

// extractBudget bounds the detached work. It exists because the goroutine is
// the only thing keeping this alive, and an unbounded call would outlive every
// run that ever triggered it.
const extractBudget = 5 * time.Minute

// History is how many documents the merge pass sees. Two is a working value,
// not a derivation: the probe showed a merge of one prior and one fresh
// reading preserved durable fields verbatim, advanced Mind state, and promoted
// two new patterns that neither input stated on its own. A third prior was not
// tested and is not claimed.
const History = 2

// Document is one bond reading, with the context it came from.
type Document struct {
	At      time.Time `json:"at"`
	Session string    `json:"session,omitempty"`
	Content string    `json:"content"`
}

// store is the rolling window: the last History documents, oldest first.
type store struct {
	path string
	mu   sync.Mutex
}

// file is the on-disk shape. A list, so appending is a read-modify-write of
// one small file rather than a directory of loose documents nobody prunes.
type file struct {
	Documents []Document `json:"documents"`
}

// NewStore returns the store for a given bond file. The history lives beside
// it, named by suffix, so an operator who deletes BOND.md to reset also knows
// there is one more file to delete.
func NewStore(bondPath string) *store {
	return &store{path: strings.TrimSuffix(bondPath, ".md") + ".history.json"}
}

// Recent returns up to History documents, oldest first.
func (s *store) Recent() ([]Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

func (s *store) read() ([]Document, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		// A corrupt window is not worth refusing a fresh reading over: the
		// alternative is a bond that can never be updated again.
		return nil, nil
	}
	if len(f.Documents) > History {
		f.Documents = f.Documents[len(f.Documents)-History:]
	}
	return f.Documents, nil
}

// Push appends a document and trims the window to History.
func (s *store) Push(doc Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, _ := s.read()
	trimmed := append(existing, doc)
	if len(trimmed) > History {
		trimmed = trimmed[len(trimmed)-History:]
	}
	encoded, err := json.Marshal(file{Documents: trimmed})
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, encoded)
}

// writeFileAtomic commits through a temp file in the same directory, so a
// crash mid-write leaves the previous window intact rather than a half file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Extract runs the two passes over one context and returns the reconciled
// document.
//
// Pass one reads the context alone. Pass two reconciles that reading with the
// window. With an empty window it is skipped rather than faked, so the first
// round is genuinely a cold read and not a merge with an imaginary prior.
func Extract(ctx context.Context, cfg llm.ProviderConfig, prior []Document, context string) (string, error) {
	context = strings.TrimSpace(context)
	if context == "" {
		return "", fmt.Errorf("bond: empty context")
	}
	fresh, err := ask(ctx, cfg, "Here is the conversation.\n\n"+context)
	if err != nil {
		return "", fmt.Errorf("bond: first pass: %w", err)
	}
	if len(prior) == 0 {
		return clean(fresh), nil
	}
	var head strings.Builder
	for i, doc := range prior {
		fmt.Fprintf(&head, "DOCUMENT %d:\n%s\n\n", i+1, strings.TrimSpace(doc.Content))
	}
	head.WriteString("DOCUMENT " + fmt.Sprint(len(prior)+1) + " (fresh extraction):\n")
	head.WriteString(fresh)
	head.WriteString("\n\n" + mergeInstruction)
	merged, err := ask(ctx, cfg, head.String())
	if err != nil {
		return "", fmt.Errorf("bond: merge pass: %w", err)
	}
	return clean(merged), nil
}

// ask performs one model call. It is a variable so tests can drive both passes
// without a provider; production assigns the implementation below.
var ask = askModel

func askModel(ctx context.Context, cfg llm.ProviderConfig, payload string) (string, error) {
	client := llm.New(cfg)
	req := llm.StreamRequest{
		Messages: []llm.Item{
			{Kind: "text", Role: llm.RoleSystem, Text: ExtractorPrompt},
			{Kind: "text", Role: llm.RoleUser, Text: payload},
		},
		MaxTokens: 4096,
	}
	var out strings.Builder
	err := client.Stream(ctx, req, func(delta llm.ProviderResponse) {
		if delta.Content != "" {
			out.WriteString(delta.Content)
		}
	})
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

// clean strips a markdown fence and the chat preambles models add despite the
// prompt asking for neither. An empty or degenerate answer is refused here
// rather than committed: writing "Understood." over a working bond is worse
// than leaving the old one in place, and that is the exact failure the probe
// recorded before the prompt gained its trailing imperative.
func clean(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		if idx := strings.Index(text, "\n"); idx >= 0 {
			text = text[idx+1:]
		}
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		text = strings.TrimSpace(text)
	}
	if !strings.Contains(text, "# BOND") {
		return ""
	}
	return text
}

// contextFrom renders a compaction hook's payload into one context: the
// checkpoint summary plus the user turns from the discarded window. Tool
// traffic is left out. It is the bulk of the token cost and carries almost
// nothing about the person, since it is the agent's own actions.
func contextFrom(summary string, discarded []sessionstore.Message) string {
	var head strings.Builder
	if trimmed := strings.TrimSpace(summary); trimmed != "" {
		head.WriteString("CHECKPOINT SUMMARY:\n" + trimmed + "\n\n")
	}
	head.WriteString("USER MESSAGES:\n")
	found := false
	for _, m := range discarded {
		if m.Role != llm.RoleUser {
			continue
		}
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		found = true
		head.WriteString("USER: " + text + "\n\n")
	}
	if !found {
		head.Reset()
		head.WriteString(strings.TrimSpace(summary))
	}
	return strings.TrimSpace(head.String())
}

// Commit writes the reconciled document to the bond file and advances the
// window. The file is what the prompt renders every turn; the window is what
// the next merge reconciles against.
func Commit(bondPath string, st *store, doc Document) error {
	if strings.TrimSpace(doc.Content) == "" {
		return fmt.Errorf("bond: refusing to commit an empty document")
	}
	if err := writeFileAtomic(bondPath, []byte(strings.TrimSpace(doc.Content)+"\n")); err != nil {
		return err
	}
	return st.Push(doc)
}

// MakeLearner ties extraction to compaction and returns a hook matching
// compaction.Learn. It returns false when no extraction credential is
// configured, which keeps the bond opt-in on cost the same way memory
// extraction is: an unconfigured install compacts normally and simply does
// not maintain a bond.
func MakeLearner(main llm.ProviderConfig, bondPath string) (func(context.Context, string, []sessionstore.Message, string), bool) {
	cfg, enabled := memory.ExtractConfigFromENV(main)
	if !enabled {
		return nil, false
	}
	st := NewStore(bondPath)
	return func(ctx context.Context, sessionID string, discarded []sessionstore.Message, summary string) {
		// Read everything the hook needs before returning. ctx is the run's
		// context and it is cancelled the moment that run finishes, so a
		// goroutine touching it later would be cancelled mid-extraction.
		one := contextFrom(summary, discarded)
		if one == "" {
			return
		}
		prior, err := st.Recent()
		if err != nil {
			prior = nil
		}
		// The hook runs inline in the compaction that produced this checkpoint,
		// and that compaction is inside the run. Two model calls inline would
		// add their full latency to a turn the user is waiting on, so the work
		// moves off and the run continues on the checkpoint immediately.
		//
		// Nothing waits on it. The bond is read per turn by the prompt and the
		// write is an atomic rename, so a document landing a few seconds late
		// is read whole or not at all.
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), extractBudget)
			defer cancel()
			running.Lock()
			defer running.Unlock()
			content, err := Extract(ctx, cfg, prior, one)
			if err != nil {
				logger.Warn("bond extraction: " + err.Error())
				return
			}
			if content == "" {
				// The model acknowledged the prompt instead of answering it, or
				// returned prose. Keeping the previous bond is the safe direction:
				// the file is better than the prompt and better than an overwrite.
				logger.Warn("bond extraction produced no document; keeping the existing bond")
				return
			}
			if err := Commit(bondPath, st, Document{At: time.Now(), Session: sessionID, Content: content}); err != nil {
				logger.Warn("bond commit: " + err.Error())
			}
		}()
	}, true
}
