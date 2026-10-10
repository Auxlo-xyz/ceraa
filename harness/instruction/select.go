package instruction

import (
	"sort"
	"strconv"
	"strings"
	"sync"
)

// NoteBudget is the ceiling on one rendered instruction note.
//
// The same argument bounds the memory sidekick note and it is the same argument:
// a tool result is a worse place to spend attention than a prompt line. It is
// read next to the output the agent actually asked for, and every byte of it is
// re-sent on every later request in the session. 1200 bytes is about six
// bounded rules with room for the frame, which is enough to carry what a single
// operation needs and not enough to become the prompt this package replaced.
const NoteBudget = 1200

// SelectLimit is how many rules one note may carry before the byte budget is
// even consulted.
//
// A count and not a byte because the two fail differently. The budget drops the
// weakest rules, and a thousand ninety-byte explanatory rules fit inside it
// while still being a page; a count is what stops that.
const SelectLimit = 8

// Matcher decides whether an Applies glob covers a target path.
//
// It is a dependency rather than an implementation on purpose. memory.Brief
// already answers this question, and it answers it properly: whole-path globs,
// base-name globs, directory prefixes, and a relative pattern against an
// absolute target. A second implementation here would be a second thing to keep
// correct, and the two would drift until a rule fired on one and not the other.
//
// The follow-up this shape is waiting for is one line in harness/memory:
//
//	func MatchesScope(applies, target string) bool { return Brief{Applies: applies}.matches(target) }
//
// and then the caller passes that in. Until it exists, a nil Matcher means scope
// cannot be verified, and a scoped rule is withheld rather than guessed at: a
// rule delivered at the wrong moment costs the model attention, and a rule that
// is never delivered at all is at least visibly missing rather than confidently
// in the way.
type Matcher func(applies, target string) bool

// Trigger is the operation a note would be attached to.
//
// It is built from what the operation manager already knows - the tool name, the
// paths it is about, whether it failed - so selection is a lookup against
// declared metadata rather than a retrieval step that has to be right.
type Trigger struct {
	// Tool is the tool that ran, matched against a "tool:" trigger.
	Tool string
	// Targets are the paths the operation was about. An empty list means the
	// operation touched no path, so no scoped rule can apply to it.
	Targets []string
	// Failed is whether the operation failed, matched against an "error"
	// trigger.
	Failed bool
}

// Seen is the set of rules already delivered in this run.
//
// It exists because a scoped rule is fired by its own trigger, over and over: a
// rule about harness/ reaches every read and every write beneath it, and each
// copy is written into a tool result that is re-sent for the rest of the
// session. Without this set the same sentence is paid for on every call, which
// is the flooding the byte budget exists to prevent.
//
// Mandatory rules are exempt from it. That is the whole difference between the
// two words and the only one: a mandatory rule is delivered on every operation
// that matches it, because the moment it is about to be needed is the moment it
// was written for, and an advisory rule that arrived once is a rule the model
// can be assumed to still hold. The cost is real and is worth stating - a
// mandatory rule in a busy run is copied into every matching tool result, and
// those copies are re-sent for the rest of the session. The per-note ceiling is
// the bound on that, not the dedup set.
//
// It is mutable and shared, so it carries its own lock. The runner owns one per
// run and the sidekick state beside it already has a mutex for the same reason.
type Seen struct {
	mu        sync.Mutex
	delivered map[ID]struct{}
}

// NewSeen returns an empty delivered set.
func NewSeen() *Seen {
	return &Seen{delivered: map[ID]struct{}{}}
}

// Mark records rules as delivered.
//
// A nil set is a no-op rather than a panic: a caller with no configured rules
// still calls Mark with nothing, and failing a run over an empty set is worse
// than dropping the mark.
func (s *Seen) Mark(ids ...ID) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delivered == nil {
		s.delivered = map[ID]struct{}{}
	}
	for _, id := range ids {
		if id != "" {
			s.delivered[id] = struct{}{}
		}
	}
}

// Delivered reports whether a rule has already reached the model this run.
func (s *Seen) Delivered(id ID) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.delivered[id]
	return ok
}

// Carry unions another set into this one.
//
// It is the seam a compaction epoch takes, and it is a union rather than a
// replace because the two directions are not the same. After a fold the
// transcript no longer carries the notes, so the rules behind them are eligible
// again - that is what Reset is for. What must NOT be lost is the record across
// a restart or a bridge rebuild, where the transcript still holds every note and
// a replace would deliver all of them a second time.
func (s *Seen) Carry(other *Seen) {
	if s == nil || other == nil {
		return
	}
	other.mu.Lock()
	ids := make([]ID, 0, len(other.delivered))
	for id := range other.delivered {
		ids = append(ids, id)
	}
	other.mu.Unlock()
	s.Mark(ids...)
}

// Reset empties the set, making every rule eligible again.
func (s *Seen) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = map[ID]struct{}{}
}

// Len is how many rules have been delivered.
func (s *Seen) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.delivered)
}

// Options is what Select needs beyond the event.
type Options struct {
	// Matcher is the host's path-glob matcher. A nil Matcher withholds every
	// scoped rule and still delivers the general ones.
	Matcher Matcher
	// Limit caps the rules returned. Zero or less means SelectLimit.
	Limit int
}

// Selection is what one operation would be told, and what was held back.
//
// The counts are the point of the struct. A note that stops without saying it
// stopped reads as the whole set, and the model is entitled to know it is not
// being shown everything - the same reason memory.sidekickNotice names the
// ceiling.
type Selection struct {
	Rules []Instruction
	// Matched is how many rules fired for this operation.
	Matched int
	// AlreadyDelivered is how many fired but have been delivered this run.
	AlreadyDelivered int
	// Withheld is how many were dropped for the limit or the byte budget.
	Withheld int
}

// Select returns the rules that bear on one operation, strongest first.
//
// Ordering is priority and then document order, and both are properties of the
// files rather than of a clock. A score derived from a wall clock would re-rank a
// note on a turn where nothing about the work changed, which is the exact
// volatility this package was built to remove: the prompt prefix would stop
// being stable because the order of two sentences moved.
//
// Two things are never dropped, and they are what "mandatory" means:
//
//   - a mandatory rule is not dropped in favour of a lower-ranked one, so it is
//     exempt from the byte budget while anything below it remains;
//   - a mandatory rule is not suppressed by the run's delivered set, so it
//     arrives on every matching operation rather than once per run.
//
// Two bounds still apply to it, and pretending otherwise would be the dishonest
// version of this feature: the count limit, and the rendered note's own ceiling.
// A file with more mandatory rules than fit in one note delivers the top of them
// every time and names how many it did not show, which is a file that wants
// fewer rules rather than a compiler that quietly lost them.
func Select(snap Snapshot, ev Trigger, seen *Seen, opts Options) Selection {
	limit := opts.Limit
	if limit <= 0 {
		limit = SelectLimit
	}

	var matched []Instruction
	result := Selection{}
	for _, item := range snap.Instructions {
		if !fires(item, ev, opts.Matcher) {
			continue
		}
		result.Matched++
		if item.Priority != Mandatory && seen.Delivered(item.ID) {
			result.AlreadyDelivered++
			continue
		}
		matched = append(matched, item)
	}

	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Priority.rank() != matched[j].Priority.rank() {
			return matched[i].Priority.rank() < matched[j].Priority.rank()
		}
		// Document order within a priority. The sources are merged in delivery
		// order by the store, so this is stable across recompiles and does not
		// depend on map iteration anywhere.
		if matched[i].Source != matched[j].Source {
			return matched[i].Source < matched[j].Source
		}
		return matched[i].Line < matched[j].Line
	})

	used := 0
	for _, item := range matched {
		cost := len(lineOf(item))
		// The budget drops from the bottom and never from the top, so a rule
		// tagged mandatory survives while anything below it is still to come.
		overBudget := item.Priority != Mandatory && used+cost > NoteBudget
		if len(result.Rules) >= limit || overBudget {
			result.Withheld++
			continue
		}
		result.Rules = append(result.Rules, item)
		used += cost
	}
	return result
}

// fires reports whether one rule applies to one operation.
//
// Every trigger has to hold, and a trigger this package cannot evaluate
// withholds the rule rather than passing it. A rule whose stated conditions were
// never checked is not a rule that has been satisfied, and delivering it anyway
// is the failure that teaches a model to ignore the tag.
func fires(item Instruction, ev Trigger, match Matcher) bool {
	for _, trigger := range item.Triggers {
		switch {
		case trigger == "error":
			if !ev.Failed {
				return false
			}
		case strings.HasPrefix(trigger, "tool:"):
			name := strings.TrimPrefix(trigger, "tool:")
			if name == "" || !strings.EqualFold(name, ev.Tool) {
				return false
			}
		default:
			// An unrecognised trigger name. Withheld, and the comment above says
			// why: the honest reading of a condition nobody can evaluate is that
			// the condition was not met.
			return false
		}
	}
	if item.Applies == "" {
		return true
	}
	if match == nil {
		return false
	}
	for _, target := range ev.Targets {
		if match(item.Applies, target) {
			return true
		}
	}
	return false
}

// Annotate renders the note appended to one tool result, or "" when there is
// nothing to say.
//
// A frame around nothing is worse than no note: it spends the bytes and tells
// the model there is something here to read.
func Annotate(ev Trigger, selected Selection) string {
	if len(selected.Rules) == 0 {
		return ""
	}
	lines := make([]string, 0, len(selected.Rules)+1)
	for _, item := range selected.Rules {
		lines = append(lines, lineOf(item))
	}
	body := strings.Join(lines, "\n") + "\n"

	// Bounded here, at render time, and never by the caller. This string is what
	// goes into the tool result the model reads and into every later request in
	// the session, so trimming it afterwards would edit bytes that have already
	// been stored and priced.
	//
	// Whole lines only. A rule cut here is a rule the model will obey as if it
	// were whole.
	for len(annotatePreamble(ev)+body+annotateClosing+noticeFor(selected.Withheld)) > NoteBudget {
		selected.Rules = selected.Rules[:len(selected.Rules)-1]
		selected.Withheld++
		body = strings.Join(lines[:len(selected.Rules)], "\n") + "\n"
		if len(selected.Rules) == 0 {
			return ""
		}
	}

	var b strings.Builder
	b.WriteString(annotatePreamble(ev))
	b.WriteString(body)
	if selected.Withheld > 0 {
		b.WriteString(noticeFor(selected.Withheld))
	}
	b.WriteString(annotateClosing)
	return b.String()
}

// lineOf renders one rule as one line, with the provenance that makes it
// checkable.
func lineOf(item Instruction) string {
	line := "- " + item.Text
	switch item.Priority {
	case Mandatory:
		line = "- MUST: " + item.Text
	case Advisory:
		line = "- SHOULD: " + item.Text
	}
	return line + " (" + item.Source + ":" + strconv.Itoa(item.Line) + ")"
}

func annotatePreamble(ev Trigger) string {
	name := "operation"
	if len(ev.Targets) == 1 {
		name = ev.Targets[0]
	} else if len(ev.Targets) > 1 {
		name = ev.Targets[0] + " +" + strconv.Itoa(len(ev.Targets)-1) + " more"
	}
	if ev.Tool != "" {
		name = ev.Tool + " " + name
	}
	return "\n\n<project_instructions note=\"" + name + "\">\n"
}

// annotateClosing says what the block is and, more importantly, what it is not.
//
// The second sentence is the one this package exists to be honest about. A rule
// delivered here is a sentence in a tool result; nothing checks it, and the
// guards that actually refuse a write or a dial live in code where they can.
const annotateClosing = "These are the project's own rules, delivered next to the work. They are guidance, not\n" +
	"enforcement: nothing here is checked for you, and a rule that says never do X is not a barrier.\n" +
	"</project_instructions>"

// noticeFor names the ceiling, because a bounded list that does not announce its
// own bound is a list the model will read as complete.
func noticeFor(withheld int) string {
	return "- ..." + strconv.Itoa(withheld) + " more not shown: this note is capped at " + strconv.Itoa(NoteBudget) + " bytes.\n"
}
