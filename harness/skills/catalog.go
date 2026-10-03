package skills

import (
	"fmt"
	"html"
	"sort"
	"strings"
	"unicode/utf8"
)

// CatalogPreamble tells the model what the catalog is and how to spend it.
//
// It travels with the catalog rather than living in prompt/system.txt because it
// describes the format directly above it, and a prompt that outlives the block
// it explains ends up instructing a model to call a tool that is not there.
//
// The second sentence is the one that matters: a catalog is only cheaper than
// inlining the bodies if the model knows the bodies are reachable and how.
const CatalogPreamble = "These skills are approved and available. Each line names one skill and describes what it covers. " +
	"Before starting a task a skill covers, call skill_use with that skill's name to read its full instructions."

// describeBudget bounds one description.
//
// A description is a catalogue line, not a summary of the document. Long enough
// to say when the skill applies, short enough that twenty of them still read as
// a list rather than a wall.
const describeBudget = 200

// Describe reduces a skill body to the one line that says what it is for.
//
// The store keeps only markdown, with no frontmatter, so the description is
// read out of the prose rather than declared. That is a heuristic and it is
// worth being honest about: it takes the first paragraph of prose, and falls
// back to the first heading when a skill is nothing but a heading.
//
// It prefers prose over a heading because a heading says the subject and the
// prose says when to reach for it, and the second is what decides whether the
// model calls the tool at all.
//
// A whole paragraph rather than one line, because markdown wraps and a
// description cut at the first newline is half a sentence.
func Describe(content string) string {
	var heading string
	var paragraph []string
	inFence := false
	flush := func() (string, bool) {
		if len(paragraph) == 0 {
			return "", false
		}
		return clamp(collapse(strings.Join(paragraph, " ")), describeBudget), true
	}
	for _, raw := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "```"):
			// Fence state is tracked rather than the line skipped, because the
			// line after the opening fence is the inside of an example and reads
			// exactly like prose.
			inFence = !inFence
		case inFence:
			continue
		case line == "":
			if described, ok := flush(); ok {
				return described
			}
		case strings.HasPrefix(line, "<!--"):
			continue
		case strings.HasPrefix(line, "#"):
			if heading == "" {
				heading = strings.TrimSpace(strings.TrimLeft(line, "#"))
				heading = strings.TrimSpace(strings.TrimSuffix(heading, ":"))
			}
		case strings.HasPrefix(line, "-"), strings.HasPrefix(line, "*"), strings.HasPrefix(line, "+"):
			// A bullet list is a sequence of steps, which is not a description.
			continue
		default:
			paragraph = append(paragraph, line)
		}
	}
	if described, ok := flush(); ok {
		return described
	}
	return clamp(collapse(heading), describeBudget)
}

// collapse folds every run of whitespace into one space, so a description
// lifted from a wrapped paragraph is one line.
func collapse(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// clamp bounds a string on a rune boundary, marking that it was cut.
func clamp(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return strings.TrimRight(value[:cut], " ") + "..."
}

// CurrentContent returns the body of a skill's latest version.
//
// Shared by the catalogue and the tool that serves it, so the two cannot pick
// different versions of the same skill.
func CurrentContent(skill Skill) string {
	version := LatestVersion(skill)
	for i := len(skill.Versions) - 1; i >= 0; i-- {
		if skill.Versions[i].Version == version {
			return skill.Versions[i].Content
		}
	}
	return ""
}

// RenderCatalog writes the skill index: a name and a description each, and no
// bodies.
//
// This replaced a renderer that inlined every approved body, which put the
// whole skill library into the prompt on every request. The cost of that was
// bounded but the bound was the only defence, so past a certain number of
// skills they were silently dropped and the agent simply got worse at its job.
// A name and a description costs about eighty bytes per skill; the body is one
// skill_use call away.
func RenderCatalog(skillList []Skill, maxBytes int) string {
	if maxBytes <= 0 || len(skillList) == 0 {
		return ""
	}
	sorted := append([]Skill(nil), skillList...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var out strings.Builder
	out.WriteString("<available_skills>\n")
	for _, skill := range sorted {
		version := LatestVersion(skill)
		description := Describe(CurrentContent(skill))
		if description == "" {
			// A skill with no prose and no heading still has to be listed, or the
			// model cannot know it exists and cannot ask for it. Saying so is
			// better than an empty description it would learn to ignore.
			description = "No description was recorded for this skill."
		}
		item := fmt.Sprintf("<skill name=%q version=%d>%s</skill>\n",
			html.EscapeString(skill.Name), version, html.EscapeString(description))
		if out.Len()+len(item)+len("</available_skills>\n") <= maxBytes {
			out.WriteString(item)
		}
	}
	out.WriteString("</available_skills>\n")
	if out.Len() <= maxBytes {
		return out.String()
	}
	return truncateUTF8(out.String(), maxBytes)
}
