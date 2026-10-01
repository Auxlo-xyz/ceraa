package tui

import (
	"strings"
)

// Inline Markdown in an answer, rendered.
//
// The table work showed the same thing one level down. A tool catalogue arrived
// as a Markdown table and got prose-wrapped, so every cell was cut in half and
// the pipes were the most legible thing on screen. The prose around it has the
// same problem in a quieter way: `**bold**` printed as `**bold**`, backticks
// printed as backticks, and list continuations started at the margin so the item
// they belonged to stopped looking like an item.
//
// Two markers are rendered and two are not.
//
//   - `**bold**` becomes bold. Emphasis is the one thing the theme already has,
//     and a heading is emphasis the model used as a structure.
//   - `` `code` `` becomes reverse video, which is the terminal's own
//     convention for a literal and the only one that needs no colour scheme.
//   - `*italic*` and `~~struck~~` lose their markers and stay plain. This theme
//     has no safe rendering for either: italic is not in every terminal, and
//     rendering struck text as dim would say de-emphasised, which is a different
//     claim. Leaving the asterisks on is worse than either.
//
// Code fences are handled by the caller rather than here, because a fence means
// everything inside it is literal and must not be scanned for markers at all.

// inlineMarker is one pair of delimiters and what to do with what is between.
type inlineMarker struct {
	open  string
	close string
	style style
}

type style int

const (
	stylePlain style = iota
	styleBold
	styleCode
)

// inlineMarkers, longest delimiter first.
//
// Order matters: `**` has to be tried before `*` or every bold run is read as an
// italic run with a stray asterisk beside it.
var inlineMarkers = []inlineMarker{
	{"**", "**", styleBold},
	{"~~", "~~", stylePlain},
	{"`", "`", styleCode},
	{"*", "*", stylePlain},
}

// renderInline applies the markers in s and returns it styled.
func renderInline(p Palette, s string) string {
	var b strings.Builder
	rest := s
	for {
		at := -1
		var chosen inlineMarker
		for _, m := range inlineMarkers {
			k := strings.Index(rest, m.open)
			if k < 0 {
				continue
			}
			// A single asterisk that is really the front of a bold run must not
			// win. `**` is checked first, so it is normally already chosen; this
			// is the case where it could not be and this one still looks like one.
			if m.open == "*" && strings.HasPrefix(rest[k:], "**") {
				continue
			}
			if at < 0 || k < at {
				at, chosen = k, m
			}
		}
		if at < 0 {
			b.WriteString(rest)
			return b.String()
		}
		// Markdown's own rule, and the reason `2 * 3` is not an italic run: a
		// delimiter that is followed by whitespace cannot open, and one preceded by
		// whitespace cannot close. Without it a multiplication sign eats everything
		// between two stray asterisks.
		body := rest[at+len(chosen.open):]
		if body == "" || strings.HasPrefix(body, " ") {
			b.WriteString(rest[:at+len(chosen.open)])
			rest = body
			continue
		}
		b.WriteString(rest[:at])
		// The first closer that can actually close. One preceded by whitespace does
		// not close, and a lone asterisk does not close on a character that belongs
		// to a bold run: without that, `*- **task** - delegate*` paired the leading
		// asterisk with the first asterisk of the bold and produced nonsense.
		end := -1
		for search := 0; search < len(body); {
			k := strings.Index(body[search:], chosen.close)
			if k < 0 {
				break
			}
			k += search
			closable := k > 0 && !strings.HasSuffix(body[:k], " ")
			if closable && chosen.open == "*" && strings.Contains(body[:k], "*") {
				closable = false
			}
			if closable {
				end = k
				break
			}
			search = k + 1
		}
		if end < 0 {
			// No closer. Emit the delimiter as the literal characters it is and keep
			// scanning the rest, so one stray asterisk does not switch off the bold
			// later in the same line. Giving up on the whole line was worse: it left
			// `**task**` unrendered because an unrelated `*` came before it.
			b.WriteString(rest[:at+len(chosen.open)])
			rest = body
			continue
		}
		inner := body[:end]
		switch chosen.style {
		case styleBold:
			b.WriteString(p.Bold(inner))
		case styleCode:
			b.WriteString(p.Code(inner))
		default:
			b.WriteString(inner)
		}
		rest = body[end+len(chosen.close):]
	}
}

// headingText recognises a line that is a heading rather than a sentence.
//
// `**Search**` is a heading because a model writes it that way far more often
// than it writes a whole line in bold, and `#` is recognised because some models
// do that instead.
func headingText(s string) (string, bool) {
	if strings.HasPrefix(s, "#") {
		return strings.TrimSpace(strings.TrimLeft(s, "#")), true
	}
	if len(s) > 4 && strings.HasPrefix(s, "**") && strings.HasSuffix(s, "**") {
		inner := s[2 : len(s)-2]
		if inner != "" && !strings.Contains(inner, "**") {
			return inner, true
		}
	}
	return "", false
}

// listItem recognises a bullet or a number and splits the line.
//
// The marker is replaced with the same glyph the tool lines use, so a list and a
// tool result read as the same kind of thing rather than as two systems.
func listItem(s string) (marker, rest string, ok bool) {
	for _, b := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(s, b) {
			return glyph("ok"), strings.TrimSpace(s[2:]), true
		}
	}
	// An ordered item: a short run of digits then a dot. Bounded at two digits so
	// "1.5 apples" is not read as item one.
	if i := strings.Index(s, ". "); i > 0 && i <= 2 && allDigits(s[:i]) {
		return s[:i+1], strings.TrimSpace(s[i+2:]), true
	}
	return "", "", false
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// answerBlock renders one complete line of answer prose: a heading, a list item,
// or a paragraph.
//
// The list case is the one that earns its own path. Wrapping a bullet as prose
// puts the continuation at the margin, which loses the indent that says the
// line belongs to the item above it, so the item is wrapped with its own
// hanging indent instead.
func (p Palette) answerBlock(line string, width int) []string {
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]

	if text, ok := headingText(trimmed); ok {
		if text == "" {
			return nil
		}
		return []string{indent + p.Bold(text) + "\n"}
	}

	if marker, rest, ok := listItem(trimmed); ok && rest != "" {
		prefix := indent + marker + " "
		hang := strings.Repeat(" ", visibleWidth(prefix))
		avail := width - visibleWidth(prefix)
		if avail < 8 {
			avail = 8
		}
		rows := wrapText(renderInline(p, rest), avail)
		out := make([]string, 0, len(rows))
		out = append(out, prefix+rows[0]+"\n")
		for _, row := range rows[1:] {
			out = append(out, hang+row+"\n")
		}
		return out
	}

	return wrapLines(renderInline(p, trimmed), width)
}

// wrapFenceLine wraps a line inside a code fence, keeping its indent.
//
// A fence is not wrapped at all, on the grounds that reflowing code breaks the
// alignment it exists to preserve. That is right about the indent and wrong about
// the width: an unwrapped line is handed to the terminal, which cuts it at a cell
// and drops the rest. On a narrow terminal that is the whole tail of a command
// gone, which is worse than a continuation line. So the indent is kept and the
// width is respected.
func wrapFenceLine(line string, width int) []string {
	if visibleWidth(line) <= width {
		return []string{line + "\n"}
	}
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]
	avail := width - visibleWidth(indent)
	if avail < 8 {
		avail = 8
	}
	rows := wrapText(trimmed, avail)
	out := make([]string, 0, len(rows))
	for i, r := range rows {
		if i == 0 {
			out = append(out, indent+r+"\n")
			continue
		}
		out = append(out, indent+r+"\n")
	}
	return out
}
