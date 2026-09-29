package memory

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Delivery selects how memory reaches the model.
type Delivery string

const (
	// DeliveryPrompt puts every above-floor memory in a <memory> block after the
	// cache seam, so it is rebuilt and re-read on every turn whether or not it
	// bears on the work. This is what Ceraa did before the sidekick existed.
	DeliveryPrompt Delivery = "prompt"
	// DeliverySidekick suppresses the block entirely and annotates tool results
	// instead, so a memory is delivered at the point of use and the system prompt
	// stays static.
	//
	// The system prompt becoming fully static is half the win: every part of it
	// moves before the seam, including the date, so the whole prompt is one
	// cached block and memory costs nothing on a turn where nothing fires.
	DeliverySidekick Delivery = "sidekick"
)

// ParseDelivery maps a configured value onto a mode, defaulting to the prompt
// path so an unset or misspelt value keeps today's behaviour rather than silently
// changing where memory goes.
func ParseDelivery(value string) Delivery {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(DeliverySidekick), "tool", "tool_output", "tools":
		return DeliverySidekick
	default:
		return DeliveryPrompt
	}
}

// SidekickGate is the weight a memory must reach to be injected at all.
//
// Higher than the render floor on purpose, because a tool result is a worse place
// to spend attention than a prompt line. The prompt block is read once with full
// context; an annotation competes with the tool output the agent actually asked
// for, and every unnecessary line there is a distraction paid for on every
// subsequent turn because it lands in the transcript.
const SidekickGate = 0.40

// sidekickGeneralLimit caps the always-applicable preferences delivered per run.
//
// General preferences have no `applies`, so they match every operation. Injecting
// them on every tool result would repeat them into the transcript once per call -
// forty copies in a long run, against a prompt block that carried them once. They
// are therefore delivered once per run, on the first tool result, and this is that
// cap.
const SidekickGeneralLimit = 3

// sidekickTargetLimit caps the targeted memories for a single operation.
const SidekickTargetLimit = 4

// sidekickWhyBytes bounds the optional rationale, so a memory cannot smuggle a
// paragraph into every matching result.
const sidekickWhyBytes = 160

// Brief is one memory rendered for injection.
type Brief struct {
	Applies string
	Why     string
	Content string
	Score   float64
	Seen    int
}

// matches reports whether this memory bears on the given target.
//
// A memory with no `applies` is general and matches nothing here; general
// memories are delivered once per run by SelectGeneral rather than on every
// operation, because a standing preference is not a response to any one call.
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

// Select returns the targeted memories for one operation, strongest first.
func Select(entries []Entry, target string, now time.Time, limit int) []Brief {
	if target == "" || limit <= 0 {
		return nil
	}
	var out []Brief
	for _, entry := range entries {
		if entry.Applies == "" {
			continue
		}
		weight := score(entry, now)
		if weight < SidekickGate {
			continue
		}
		brief := briefOf(entry, weight)
		if brief.matches(target) {
			out = append(out, brief)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// SelectGeneral returns the strongest always-applicable memories, for delivery
// once per run.
func SelectGeneral(entries []Entry, now time.Time, limit int) []Brief {
	if limit <= 0 {
		return nil
	}
	var out []Brief
	for _, entry := range entries {
		if entry.Applies != "" {
			continue
		}
		weight := score(entry, now)
		if weight < SidekickGate {
			continue
		}
		out = append(out, briefOf(entry, weight))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ScoreOf exposes an entry's current weight to callers outside this package.
func ScoreOf(entry Entry, now time.Time) float64 { return score(entry, now) }

func briefOf(entry Entry, weight float64) Brief {
	return Brief{
		Applies: entry.Applies,
		Why:     entry.Why,
		Content: entry.Content,
		Score:   weight,
		Seen:    entry.Seen,
	}
}

// Annotate renders the block appended to a tool result.
//
// The marker is load-bearing twice over. It tells the model this is guidance
// rather than something the tool returned, which matters because the whole idea
// is to place instruction next to the work it concerns; and it makes the block
// findable by the memory tooling when the agent chooses to confirm or contradict
// something it was just shown.
func Annotate(target string, targeted, general []Brief) string {
	if len(targeted) == 0 && len(general) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n<sidekick note=\"")
	if target != "" {
		b.WriteString(escapeAttr(target))
	} else {
		b.WriteString("start-of-run")
	}
	b.WriteString("\">\n")
	if len(general) > 0 {
		b.WriteString("Standing preferences already known about this project:\n")
		for _, item := range general {
			b.WriteString("- " + oneLine(item) + "\n")
		}
	}
	if len(targeted) > 0 {
		b.WriteString("Known about " + escapeAttr(target) + ":\n")
		for _, item := range targeted {
			line := "- " + oneLine(item)
			if item.Why != "" {
				line += " (because: " + oneLine(Brief{Content: item.Why}) + ")"
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("These are things already established, not new instructions. If one is\n")
	b.WriteString("wrong or the user reverses it, say so and it will be corrected.\n")
	b.WriteString("</sidekick>")
	return b.String()
}

// oneLine flattens and bounds a rendered fragment.
//
// The bound lives here rather than in briefOf because Annotate accepts a Brief
// from any caller, and a bound applied only at construction is a bound a caller
// can walk straight past. Bounding at the point the text becomes output is the
// only place it cannot be bypassed.
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
	Mode             string `json:"mode"`
	PromptBlock      bool   `json:"prompt_block"`
	GeneralInjected  int    `json:"general_injected"`
	TargetedInjects  int    `json:"targeted_injections"`
	SuppressedByGate int    `json:"suppressed_by_gate"`
}

// NoteSuppressed records what the gate held back.
func (r *DeliveryReport) NoteSuppressed(gate int) {
	if r == nil {
		return
	}
	r.SuppressedByGate = gate
}

func (r *DeliveryReport) String() string {
	if r == nil {
		return "memory delivery: none"
	}
	return fmt.Sprintf("memory delivery: mode=%s prompt_block=%v general=%d targeted=%d below_gate=%d",
		r.Mode, r.PromptBlock, r.GeneralInjected, r.TargetedInjects, r.SuppressedByGate)
}
