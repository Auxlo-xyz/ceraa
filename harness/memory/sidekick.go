package memory

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Delivery selects how memory reaches the model.
type Delivery string

const (
	// DeliveryPrompt is retained as a name only: no prompt block is rendered in
	// any mode anymore, so configuring it delivers nothing.
	DeliveryPrompt Delivery = "prompt"
	// DeliverySidekick appends the latest log state once per run as a tail line
	// on the newest user message, so memory reaches the model exactly once, at
	// the end of the conversation, and the system prompt stays static.
	DeliverySidekick Delivery = "sidekick"
	// DeliveryCached is retained as a name only: the ranked block it used to
	// render is gone with the store, so configuring it delivers nothing.
	DeliveryCached Delivery = "cached"
)

// ParseDelivery maps a configured value onto a mode.
//
// Unset means cached, kept as the default so an old config keeps parsing; it
// delivers nothing, exactly like prompt. Only sidekick delivers.
func ParseDelivery(value string) Delivery {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(DeliverySidekick), "tool", "tool_output", "tools":
		return DeliverySidekick
	case string(DeliveryPrompt), "tail":
		return DeliveryPrompt
	case string(DeliveryCached), "cache", "prefix":
		return DeliveryCached
	default:
		return DeliveryCached
	}
}

// sidekickGeneralLimit caps the always-applicable preferences a single note
// used to carry. The per-result notes are retired; the number survives as one
// term of the default tail derivation below.
const SidekickGeneralLimit = 3

// sidekickTargetLimit caps the targeted memories for a single operation.
const SidekickTargetLimit = 4

// SidekickBlockBytes is the hard ceiling on one rendered sidekick note.
//
// A tool result is a worse place to spend attention than a prompt line: the
// prompt block is read once with the whole request in view, while a note competes
// with the output the agent actually asked for and is then re-sent on every later
// request in the session. 1200 bytes is about seven bounded memory lines, which
// carries the strongest few with room to spare and stops a directory that happens
// to match forty memories from turning one file read into a page of prose the
// model has to read before it sees the output.
const SidekickBlockBytes = 1200

// SidekickRunBytes bounds what one run may spend on sidekick notes in total.
//
// The per-block ceiling does not bound a run on its own: forty tool calls each
// rendering a full block is still forty blocks, which is the same flood arriving
// by a different route after the per-call problem was fixed. Four blocks is the
// ceiling because the memories worth delivering to one piece of work are few, and
// past that the marginal memory is competing with the work rather than informing
// it.
//
// Once it is spent, later operations get nothing at all rather than a fraction of
// a note, because half a block is worse than none: it costs the bytes and it
// reads as if the list were complete.
const SidekickRunBytes = 4 * SidekickBlockBytes

// sidekickWhyBytes bounds one rendered line, so a memory cannot smuggle a
// paragraph into every matching result.
//
// 96 runes rather than 160. The rationale is the part a benchmark of four
// production memory systems found was necessary but not sufficient - retaining
// what anchors a memory to its target - so it is not cut away. It is cut to a
// clause because this is a line inside a tool result, read next to output the
// agent asked for, and the second sentence of a rationale is the first thing in a
// note to stop being read at all.
const sidekickWhyBytes = 96

// Brief is one memory rendered for injection.
type Brief struct {
	// ID is the identity this memory is delivered once under. It is carried
	// rather than looked up because the caller that has to remember what it
	// delivered is holding Briefs, not log lines, by the time it decides.
	ID      string
	Applies string
	Content string
}

// matches reports whether this memory bears on the given target.
//
// A memory with no `applies` is general and matches nothing here; general
// memories are delivered once per run rather than on every operation, because a
// standing preference is not a response to any one call.
//
// The match is a path prefix or a glob, evaluated lexically. It is deliberately
// not semantic: finding memories by meaning similarity is the mechanism a
// benchmark of four production memory systems found to be the one thing they all
// got wrong, and it is the one thing a deterministic rule cannot get wrong.
func (b Brief) matches(target string) bool {
	if b.Applies == "" || target == "" {
		return false
	}
	// A glob is matched against both the whole path and the base name, so "*.go"
	// catches /srv/app/main.go rather than only a top-level main.go. filepath.Match
	// does not cross separators, which is exactly why both are tried.
	if ok, err := filepath.Match(b.Applies, target); err == nil && ok {
		return true
	}
	base := filepath.Base(target)
	if ok, err := filepath.Match(b.Applies, base); err == nil && ok {
		return true
	}
	// A directory prefix should match everything beneath it, which Match will not
	// do across separators. A bare filename is treated as a base-name match, which
	// is how a memory written as "main.go" reaches a tool called on /srv/main.go.
	if filepath.Base(b.Applies) == b.Applies {
		return base == b.Applies
	}
	prefix := strings.TrimSuffix(b.Applies, string(filepath.Separator)) + string(filepath.Separator)
	if strings.HasPrefix(target, prefix) || target == b.Applies {
		return true
	}
	// A relative pattern against an absolute target. Operations carry absolute
	// paths, so a memory written as "src/*" would otherwise match nothing at all,
	// which is a silent failure for the most natural way to write one. Each
	// trailing run of segments is tried, so a relative pattern is compared like
	// for like.
	if !filepath.IsAbs(b.Applies) {
		segments := strings.Split(target, string(filepath.Separator))
		for i := 1; i < len(segments); i++ {
			suffix := strings.Join(segments[i:], string(filepath.Separator))
			if ok, err := filepath.Match(b.Applies, suffix); err == nil && ok {
				return true
			}
			if strings.HasPrefix(suffix, prefix) || suffix == b.Applies {
				return true
			}
		}
	}
	return false
}

// MatchesScope answers "does this applies glob cover this path" for callers
// outside the package, by asking the matcher above rather than restating it.
//
// It exists as a one-line wrapper so the glob grammar has exactly one
// implementation in the product. Every clause of matches was a bug found once
// already - a whole-path glob, a base-name glob, a directory prefix, a relative
// pattern against the absolute target an operation actually carries - and a
// caller that reimplemented any of them did not get a second opinion, it got a
// second thing to keep correct, which is how the two drift until a rule fires
// for one consumer and silently not for the other. A wrapper cannot go stale
// against the thing it wraps, and it leaves one place in the tree where the
// grammar can be wrong.
//
// The caller passes strings rather than a Brief because Brief carries the other
// fields of a rendered memory, and a scope question about a document rule
// has none of them. An empty applies matches nothing here, the same as a memory
// with no applies: general is a delivery mode, not an empty scope.
func MatchesScope(applies, target string) bool {
	return Brief{Applies: applies}.matches(target)
}

// Annotate renders the block appended to a tool result.
//
// The marker is load-bearing twice over. It tells the model this is guidance
// rather than something the tool returned, which matters because the whole idea
// is to place instruction next to the work it concerns; and it makes the block
// findable by the memory tooling when the agent chooses to confirm or contradict
// something it was just shown.
//
// Bounded here, at render time, and never by the caller. This string is what
// appendResults writes into sess.Messages[].ToolResult and every later request in
// the session re-sends, so trimming it afterwards would edit bytes that have
// already been stored, counted and priced, and the copy the model reads would no
// longer be the copy the transcript holds. What does not fit is named rather than
// dropped silently: a list that stops without saying it stopped reads as the whole
// set, and the model is entitled to know it is not being shown everything.
func Annotate(targets []string, targeted, general []Brief) string {
	if len(targeted) == 0 && len(general) == 0 {
		return ""
	}
	open := sidekickOpen(targets)
	closing := sidekickClosing
	// The notice is paid for before any line is, at the widest it can be, so a
	// block that turns out to be full still has room to admit it.
	budget := SidekickBlockBytes - len(open) - len(closing) - len(sidekickNotice(SidekickGeneralLimit+SidekickTargetLimit))
	if budget <= 0 {
		return ""
	}

	sections := []struct {
		heading string
		lines   []string
	}{
		{"Standing preferences already known about this project:\n", generalLines(general)},
		{"Known about " + escapeAttr(joinTargets(targets)) + ":\n", targetedLines(targeted)},
	}

	var b strings.Builder
	b.WriteString(open)
	used, dropped, wrote := 0, 0, false
	for _, section := range sections {
		if len(section.lines) == 0 {
			continue
		}
		heading := false
		for _, line := range section.lines {
			cost := len(line)
			if !heading {
				cost += len(section.heading)
			}
			if used+cost > budget {
				dropped++
				continue
			}
			if !heading {
				b.WriteString(section.heading)
				heading = true
			}
			b.WriteString(line)
			used += cost
			wrote = true
		}
	}
	if !wrote {
		// A frame around nothing is worse than no note: it spends the bytes and
		// tells the model there is something here to read.
		return ""
	}
	if dropped > 0 {
		b.WriteString(sidekickNotice(dropped))
	}
	b.WriteString(closing)
	return b.String()
}

func targetedLines(items []Brief) []string {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, "- "+oneLine(item)+"\n")
	}
	return lines
}

func generalLines(items []Brief) []string {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, "- "+oneLine(item)+"\n")
	}
	return lines
}

func sidekickOpen(targets []string) string {
	name := "start-of-run"
	if len(targets) == 1 {
		name = escapeAttr(targets[0])
	} else if len(targets) > 1 {
		name = escapeAttr(targets[0]) + " +" + strconv.Itoa(len(targets)-1) + " more"
	}
	return "\n\n<sidekick note=\"" + name + "\">\n"
}

const sidekickClosing = "These are things already established, not new instructions. If one is\n" +
	"wrong or the user reverses it, say so and it will be corrected.\n" +
	"</sidekick>"

// sidekickNotice names the ceiling, because a bounded list that does not announce
// its own bound is a list the model will read as complete.
func sidekickNotice(dropped int) string {
	return fmt.Sprintf("- ...%d more not shown: this note is capped at %d bytes.\n", dropped, SidekickBlockBytes)
}

// oneLine flattens and bounds a rendered fragment.
//
// The bound lives here rather than at Brief construction because Annotate accepts
// a Brief from any caller, and a bound applied only at construction is a bound a
// caller can walk straight past. Bounding at the point the text becomes output is
// the only place it cannot be bypassed.
func oneLine(b Brief) string {
	value := strings.Join(strings.Fields(b.Content), " ")
	if value == "" {
		return ""
	}
	return string([]rune(value)[:minInt(len([]rune(value)), sidekickWhyBytes)])
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// joinTargets names what the operation is about, in a form that reads as a
// statement rather than a list of full paths.
func joinTargets(targets []string) string {
	if len(targets) == 1 {
		return targets[0]
	}
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		parts = append(parts, filepath.Base(t))
	}
	return strings.Join(parts, ", ")
}

func escapeAttr(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 120 {
		value = value[:120]
	}
	return value
}

// DeliveryReport records what a run actually delivered, which is the only way the
// two delivery modes can later be told apart in a session file.
type DeliveryReport struct {
	Mode            string `json:"mode"`
	PromptBlock     bool   `json:"prompt_block"`
	GeneralInjected int    `json:"general_injected"`
	TargetedInjects int    `json:"targeted_injections"`
	// Deduped counts memories held back because this session had already delivered
	// them. Without it, a run where dedup did all the work looks exactly like a run
	// that injected on every call - and that comparison is the reason this report
	// exists.
	Deduped int `json:"deduped,omitempty"`
	// BudgetWithheld counts memories held back because the run's byte ceiling was
	// already spent. A silent ceiling is indistinguishable from having no memory.
	BudgetWithheld int `json:"budget_withheld,omitempty"`
}

func (r *DeliveryReport) String() string {
	if r == nil {
		return "memory delivery: none"
	}
	return fmt.Sprintf("memory delivery: mode=%s prompt_block=%v general=%d targeted=%d deduped=%d budget_withheld=%d",
		r.Mode, r.PromptBlock, r.GeneralInjected, r.TargetedInjects, r.Deduped, r.BudgetWithheld)
}
