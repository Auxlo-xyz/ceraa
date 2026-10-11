// Package bond maintains the identity section of the append-only Memory.md
// log.
//
// It differs from project memory in scope and in ownership. Memory is
// per-project and the model mines it. The bond is one document about one
// person across every project, and it stays advisory: the model writes only
// what a compaction checkpoint supports, and Commit refuses anything else.
//
// It used to own a file. BOND.md was rewritten in place at every compaction
// through a versioned store that carried the last two documents in a sidecar
// window, and harness/memory rendered it back into a <bond> prompt block. That
// was two mechanisms for one concept, and the rewrite was the half that could
// lose data: a commit replaced the only current version, so one bad round
// destroyed a good bond and the window beside it was the only recovery. Both
// are deleted. The identity document is now ONE entry in the log, which is its
// own history, and delivery is the sidekick's - out of the same tail every
// other memory is read from.
//
// What transfers is the extraction, unchanged: two passes over a compaction
// checkpoint, the second reconciling the fresh reading against the documents
// already in the log. The rolling window is read back out of the log rather
// than kept beside it, because an append-only log IS the history. There is no
// .history file, no atomic rewrite and no in-place overwrite left to maintain.
//
// Triggered off compaction, not off turns: a checkpoint is the only place the
// transcript has been deliberately condensed, so it is the only place a
// context worth summarizing exists, and it costs nothing per turn.
//
// The name survives the change. Everything in here is identity maintenance for
// the log, and the package still maintains one identity document - it just no
// longer maintains it in a file of its own. A rename would pull every caller
// in internal/ into the same edit for no reader's benefit.
package bond

import (
	"context"
	"fmt"
	"os"
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

// History is how many identity documents the merge pass sees. Two is a working
// value, not a derivation: the probe showed a merge of one prior and one fresh
// reading preserved durable fields verbatim, advanced Mind state, and promoted
// two new patterns that neither input stated on its own. A third prior was not
// tested and is not claimed.
//
// It used to bound a sidecar window file. It now bounds how far back into the
// log the window is read, and the log keeps every entry whether or not the
// merge pass looks at it.
const History = 2

// Document is one identity reading: the text the model produced and the
// session it came from.
//
// It carried a timestamp as well, and that field is gone rather than unused.
// The log stamps its own date per entry - "the date is written rather than read
// back" - so a document no longer knows when it was made; the entry does, and
// an entry is the only copy there is.
type Document struct {
	Session string
	Content string
}

// identityHeading is what every identity entry carries.
//
// It is the same marker clean() validates against, and that is what makes the
// two halves agree: an entry cannot reach the log without it, so an entry
// found by it is an entry that was validated on the way in. Reading the window
// back out of the log is the whole of how the rolling merge survives with no
// sidecar file, and it rests on this marker being both required on the way in
// and looked for on the way out.
const identityHeading = "# BOND"

// identityWindow returns the last History identity documents in the log,
// oldest first, for the merge pass.
//
// The log carries every kind of entry, so the window is a filter and not a
// slice. At one compaction the log receives identity entries, project memories
// and the checkpoint summary, and taking the last two entries blind would hand
// the merge pass a summary and a memory and call them a bond.
//
// It reads the whole log, because a fixed-size tail is not the last two
// identity entries unless the identity entries happen to be at the end, and
// they are not. This runs once per compaction, so the cost lands on the rarest
// event in the system rather than on a turn.
func identityWindow(log *memory.MemoryFile) []Document {
	if log == nil {
		return nil
	}
	var window []Document
	for _, entry := range log.Tail(log.Lines()) {
		if !strings.Contains(entry, identityHeading) {
			continue
		}
		window = append(window, Document{Content: entryBody(entry)})
		if len(window) > History {
			window = window[1:]
		}
	}
	return window
}

// entryBody returns an entry's document without the log's date line.
//
// The log frames every entry as "## <date> <session>" followed by the text,
// and only the text is the document. The date is deliberately left behind
// rather than carried into the merge payload: the fresh reading is already
// labelled as fresh and ordered last, so a date in the payload would be a
// second, weaker statement of the same fact.
func entryBody(entry string) string {
	if i := strings.IndexByte(entry, '\n'); i >= 0 {
		return strings.TrimSpace(entry[i+1:])
	}
	return strings.TrimSpace(entry)
}

// Extract runs the two passes over one context and returns the reconciled
// document.
//
// Pass one reads the context alone. Pass two reconciles that reading with the
// window. With an empty window it is skipped rather than faked, so the first
// round is genuinely a cold read and not a merge with an imaginary prior.
//
// The window is read back out of the log by identityWindow rather than held in
// this process, so a restart does not reset the merge: the previous run's
// reading is still the last identity entry in the file.
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
//
// It now runs on the way into the log as well as out of Extract, because the
// two are not the same call: a commit is the only place an entry is created,
// and a validator that runs somewhere upstream of it is a validator a later
// caller can route around.
func clean(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		if idx := strings.Index(text, "\n"); idx >= 0 {
			text = text[idx+1:]
		}
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		text = strings.TrimSpace(text)
	}
	if !strings.Contains(text, identityHeading) {
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

// Commit appends one document to the log as a single entry.
//
// Appending is the whole of what a commit is now, and it is the behavioural
// change: there is no current version to overwrite, so a round that produces a
// worse document leaves the better one above it in the log rather than
// destroying it. The versioned store this replaces kept exactly one current
// copy, which is why one bad round could cost the whole bond.
//
// The document is validated here rather than trusted. Extract already cleans
// its own output, but Extract's output and Commit's input are not the same
// call, and a fence the caller walks around is not a fence. Writing
// "Understood." into the log is worse than writing nothing.
func Commit(log *memory.MemoryFile, doc Document) error {
	if log == nil {
		return fmt.Errorf("bond: no log to commit to")
	}
	body := clean(doc.Content)
	if body == "" {
		return fmt.Errorf("bond: refusing to commit an empty document")
	}
	return log.Append(body, doc.Session)
}

// Migrate carries a legacy BOND.md into the log as one entry, once.
//
// It exists because deleting the renderer would otherwise strand every
// install's identity document: the file is still sitting in the data
// directory, nothing reads it any more, and the only way its content reaches
// the log is a step somebody has to take. A library that strands its users'
// existing data is a library that gets reverted, so the step is code rather
// than a line in a changelog.
//
// It is idempotent by the log rather than by a marker file. If the log already
// carries an identity entry then the migration has run, and this appends
// nothing: a second marker would be a second thing to lose, and losing it
// duplicates the document while keeping it appends nothing at all.
//
// A legacy file with no identity in it is refused rather than migrated, so a
// stray note left in the data directory does not become a bond entry.
func Migrate(log *memory.MemoryFile, legacyPath string) (bool, error) {
	if log == nil || strings.TrimSpace(legacyPath) == "" {
		return false, nil
	}
	raw, err := os.ReadFile(legacyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	body := clean(string(raw))
	if body == "" {
		return false, nil
	}
	if len(identityWindow(log)) > 0 {
		return false, nil
	}
	return true, log.Append(body, "")
}

// MakeLearner ties identity extraction to compaction and returns a hook
// matching compaction.Learn.
//
// The log is passed in rather than derived, because every caller already has
// one: Memory.md is opened once per run and shared with the other learners and
// the sidekick. A path parameter here would be a second answer to "where does
// this project's log live", and the signature it replaces took a bond path for
// exactly that reason - which is how the bond ended up writing a file nothing
// read.
//
// It returns false when no extraction credential is configured, which keeps
// the bond opt-in on cost the same way memory extraction is: an unconfigured
// install compacts normally and simply does not maintain an identity section.
func MakeLearner(main llm.ProviderConfig, log *memory.MemoryFile) (func(context.Context, string, []sessionstore.Message, string), bool) {
	cfg, enabled := memory.ExtractConfigFromENV(main)
	if !enabled || log == nil {
		return nil, false
	}
	return func(ctx context.Context, sessionID string, discarded []sessionstore.Message, summary string) {
		// Read everything the hook needs before returning. ctx is the run's
		// context and it is cancelled the moment that run finishes, so a
		// goroutine touching it later would be cancelled mid-extraction.
		one := contextFrom(summary, discarded)
		if one == "" {
			return
		}
		// The window is read here, off the run's context, so a compaction
		// racing this one cannot hand the merge pass a window that changes
		// underneath it while the passes are in flight.
		prior := identityWindow(log)
		// The hook runs inline in the compaction that produced this checkpoint,
		// and that compaction is inside the run. Two model calls inline would
		// add their full latency to a turn the user is waiting on, so the work
		// moves off and the run continues on the checkpoint immediately.
		//
		// Nothing waits on it. The log is appended to rather than rewritten, so
		// an entry landing a few seconds late is read whole or not at all, and
		// a late entry is a later one in the same file rather than a lock
		// somebody has to take.
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
				// returned prose. Appending nothing is the safe direction: the
				// entries already in the log are better than a new one that
				// records an acknowledgement.
				logger.Warn("bond extraction produced no document; keeping the existing identity entries")
				return
			}
			if err := Commit(log, Document{Session: sessionID, Content: content}); err != nil {
				logger.Warn("bond commit: " + err.Error())
			}
		}()
	}, true
}
