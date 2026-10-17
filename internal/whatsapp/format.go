package whatsapp

import (
	"strings"
	"unicode/utf8"
)

// waTableMaxWidth bounds a rendered table's display width in columns. A
// WhatsApp code block wraps before a phone screen fits much more than this, and
// a wrapped table loses the alignment that was the only reason to render it.
const waTableMaxWidth = 40

// formatTables rewrites GitHub-flavoured pipe tables into monospace code blocks
// with columns padded to equal width.
//
// WhatsApp has no table syntax: a pipe table arrives as literal `|` and `---`,
// which is what a reader sees today. It does render a triple-backtick block in
// monospace on every client, so a padded grid lines up there. A table wider than
// waTableMaxWidth is left exactly as it was, because it cannot align inside the
// block and converting it would only move the breakage into a fence.
//
// Only tables are touched. Bold, headings and links are a separate change.
func formatTables(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for i := 0; i < len(lines); {
		if isFenceLine(lines[i]) {
			inFence = !inFence
			out = append(out, lines[i])
			i++
			continue
		}
		if !inFence && i+1 < len(lines) && startsTable(lines[i], lines[i+1]) {
			end := i + 2
			for end < len(lines) && isTableRow(lines[end]) && !isDelimiterRow(lines[end]) {
				end++
			}
			if rendered, ok := renderTable(lines[i:end]); ok {
				out = append(out, rendered...)
			} else {
				out = append(out, lines[i:end]...)
			}
			i = end
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return strings.Join(out, "\n")
}

// startsTable reports whether header and delimiter open a table: a pipe row
// followed by a delimiter whose cell count matches it, so a column count that
// disagrees is not a table.
func startsTable(header, delimiter string) bool {
	return isTableRow(header) && isDelimiterRow(delimiter) && len(splitRow(header)) == len(splitRow(delimiter))
}

// isFenceLine reports whether a line opens or closes a triple-backtick block.
func isFenceLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "```")
}

// isTableRow reports whether a line carries a cell separator.
func isTableRow(line string) bool {
	return strings.Contains(line, "|")
}

// isDelimiterRow reports whether a line is the `|---|---` row under a header.
func isDelimiterRow(line string) bool {
	if !strings.Contains(line, "|") || !strings.Contains(line, "-") {
		return false
	}
	cells := splitRow(line)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		c = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(c), ":"), ":")
		if c == "" {
			return false
		}
		for _, r := range c {
			if r != '-' {
				return false
			}
		}
	}
	return true
}

// splitRow splits a table row on unescaped pipes, drops the padding cells an
// outer `|` leaves behind, and trims each cell. An escaped `\|` stays in the
// cell as a literal pipe.
func splitRow(line string) []string {
	rs := []rune(strings.TrimSpace(line))
	parts := []string{}
	var b strings.Builder
	for i := 0; i < len(rs); i++ {
		if rs[i] == '\\' && i+1 < len(rs) && rs[i+1] == '|' {
			b.WriteRune('|')
			i++
			continue
		}
		if rs[i] == '|' {
			parts = append(parts, b.String())
			b.Reset()
			continue
		}
		b.WriteRune(rs[i])
	}
	parts = append(parts, b.String())
	if len(parts) > 0 && strings.TrimSpace(parts[0]) == "" {
		parts = parts[1:]
	}
	if len(parts) > 0 && strings.TrimSpace(parts[len(parts)-1]) == "" {
		parts = parts[:len(parts)-1]
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// renderTable lays a header-plus-body block out as the lines of a fenced code
// block. It reports false when the grid would exceed waTableMaxWidth, leaving
// the caller to pass the original lines through untouched.
//
// Width is counted in runes, which is exact for Latin text and digits and only
// approximate for double-width scripts. A cell keeps its markdown verbatim, so
// an emphasis marker still counts toward its column.
func renderTable(block []string) ([]string, bool) {
	if len(block) < 2 {
		return nil, false
	}
	header := splitRow(block[0])
	if len(header) == 0 {
		return nil, false
	}
	cols := len(header)
	rows := make([][]string, 0, len(block)-2)
	for _, line := range block[2:] {
		cells := splitRow(line)
		if len(cells) == 0 {
			continue
		}
		if len(cells) > cols {
			cols = len(cells)
		}
		rows = append(rows, cells)
	}
	widths := make([]int, cols)
	tally := func(cells []string) {
		for c := 0; c < len(cells) && c < cols; c++ {
			if w := utf8.RuneCountInString(cells[c]); w > widths[c] {
				widths[c] = w
			}
		}
	}
	tally(header)
	for _, r := range rows {
		tally(r)
	}
	total := 0
	for _, w := range widths {
		total += w
	}
	if cols > 1 {
		total += 3 * (cols - 1)
	}
	if total > waTableMaxWidth {
		return nil, false
	}
	rowLine := func(cells []string) string {
		parts := make([]string, cols)
		for c := 0; c < cols; c++ {
			cell := ""
			if c < len(cells) {
				cell = cells[c]
			}
			parts[c] = cell + strings.Repeat(" ", widths[c]-utf8.RuneCountInString(cell))
		}
		return strings.TrimRight(strings.Join(parts, " | "), " ")
	}
	sep := make([]string, cols)
	for c := 0; c < cols; c++ {
		sep[c] = strings.Repeat("-", widths[c])
	}
	rendered := []string{"```", rowLine(header), strings.Join(sep, "-+-")}
	for _, r := range rows {
		rendered = append(rendered, rowLine(r))
	}
	return append(rendered, "```"), true
}
