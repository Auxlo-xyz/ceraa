package whatsapp

import (
	"strings"
	"unicode/utf8"
)

// waTableMaxWidth bounds a rendered grid's display width in columns. A WhatsApp
// code block wraps before a phone screen fits much more than this, and a wrapped
// grid loses the alignment that was the only reason to render it, so a table
// wider than this is laid out as vertical rows rather than as a grid.
const waTableMaxWidth = 40

// formatTables rewrites GitHub-flavoured pipe tables into something WhatsApp can
// show. WhatsApp has no table syntax: a pipe table arrives as literal `|` and
// `---`, which is what a reader sees today.
//
// A table that fits waTableMaxWidth becomes a fenced monospace grid, which every
// client renders in a fixed-width font so the columns line up. A wider one cannot
// align on a phone, so it is laid out as vertical rows instead of staying as raw
// pipes.
//
// Only tables are touched. Emphasis is a separate pass in formatEmphasis,
// which runs before this one; headings and links are still a separate change.
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

// renderTable lays a header-plus-body block out for WhatsApp: a fenced monospace
// grid when it fits waTableMaxWidth, otherwise vertical rows. It reports false
// only when the block carries no usable table at all.
//
// Width is counted in runes, which is exact for Latin text and digits and only
// approximate for double-width scripts. A cell may still carry emphasis marks,
// since formatEmphasis has run by the time a table reaches here, and those
// count toward its column like any other character.
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
		return renderTableVertical(header, rows, cols), true
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

// renderTableVertical lays a table that is too wide for the grid out as one
// short group per row: the first column as the row's title, then each remaining
// column as a bold `Header: value` line, with a blank line between rows. Nothing
// has to align, so a long cell such as a path wraps in the reader's own client
// instead of dragging every other column out of line.
func renderTableVertical(header []string, rows [][]string, cols int) []string {
	at := func(cells []string, c int) string {
		if c < len(cells) {
			return cells[c]
		}
		return ""
	}
	out := make([]string, 0, len(rows)*cols)
	for i, r := range rows {
		if i > 0 {
			out = append(out, "")
		}
		if title := at(r, 0); title != "" {
			out = append(out, title)
		}
		for c := 1; c < cols; c++ {
			value := at(r, c)
			if value == "" {
				continue
			}
			label := ""
			if c < len(header) {
				// The label may already carry WhatsApp's bold from the
				// emphasis pass; the wrapper below adds it again, so the
				// marks are taken back off first or the stars show.
				label = trimEmphasis(header[c])
			}
			if label == "" {
				out = append(out, value)
				continue
			}
			out = append(out, "*"+label+"*: "+value)
		}
	}
	return out
}

// formatEmphasis rewrites markdown emphasis into the marks WhatsApp reads.
//
// WhatsApp has no markdown, so "**bold**" arrives literally as stars around a
// word and a reply that looks bold in every other client looks like source
// here. The two languages cannot be bridged by a string replace because they
// disagree about the single asterisk: markdown italic is "*x*" and WhatsApp
// bold is "*x*", the same two bytes carrying different meanings. The
// translation is only sound while the source is still known to be model
// markdown, so it runs on the answer alone and never on text the bridge wrote
// itself - a tool line, a vertical table label and a fixed reply are already
// in WhatsApp's own spelling, and running them through here would rewrite
// bold as italic.
//
// Fenced blocks and inline code are left exactly as written. A star or an
// underscore inside them belongs to the program or the path rather than to
// emphasis, which is the same reason toolLine wraps a path in backticks.
//
// Only emphasis is converted. Headings, links and lists are still sent as
// markdown, a separate change that formatTables already defers to.
func formatEmphasis(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		if isFenceLine(line) {
			inFence = !inFence
			out = append(out, line)
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}
		out = append(out, convertEmphasisLine(line))
	}
	return strings.Join(out, "\n")
}

// convertEmphasisLine scans one line left to right, so a mark this function
// produced is never examined again. Repeated string replaces cannot do that,
// and a "*x*" written by the bold rule would be read back as markdown italic
// by the italic rule if they ran as separate passes.
func convertEmphasisLine(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '`' {
			n := runLen(s, i, '`')
			if close := nextRun(s, i+n, '`', n); close >= 0 {
				b.WriteString(s[i : close+n])
				i = close + n
				continue
			}
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		if s[i] != '*' && s[i] != '_' {
			b.WriteByte(s[i])
			i++
			continue
		}
		ch := s[i]
		n := runLen(s, i, ch)
		// A lone underscore is WhatsApp's italic already, and in a path it
		// is an identifier character. Leaving it costs nothing: markdown's
		// "_x_" and WhatsApp's "_x_" are the same bytes.
		if ch == '_' && n == 1 {
			b.WriteByte(s[i])
			i++
			continue
		}
		if n > 3 {
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		content, next, ok := emphasisSpan(s, i+n, ch, n)
		if !ok {
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		open, close := emphasisMark(ch, n)
		b.WriteString(open)
		b.WriteString(convertEmphasisLine(content))
		b.WriteString(close)
		i = next
	}
	return b.String()
}

// emphasisSpan finds the closing run for an opener of n copies of ch starting
// at after. It reports false for a run with no close, an empty span, or one
// padded with space, which is how a bullet line like "* item" and a stray
// asterisk are told apart from real emphasis.
func emphasisSpan(s string, after int, ch byte, n int) (content string, next int, ok bool) {
	close := nextRun(s, after, ch, n)
	if close < 0 {
		return "", 0, false
	}
	content = s[after:close]
	if content == "" || content[0] == ' ' || content[0] == '\t' ||
		content[len(content)-1] == ' ' || content[len(content)-1] == '\t' {
		return "", 0, false
	}
	run := runLen(s, close, ch)
	// "**a *b***" ends its inner italic on the same stars that close the
	// bold, so the closing run is one star longer than the opener and holds
	// the inner close as well. Those extra stars go into the content, where
	// the recursive call turns them into the inner span. With an even
	// number of inner stars there is no unmatched opener to close and the
	// extras stay put, ready to be scanned as the stray they are.
	if ch == '*' && run > n && oddSingleStars(content) {
		extra := run - n
		return content + s[close:close+extra], close + run, true
	}
	return content, close + n, true
}

// emphasisMark is the WhatsApp spelling of a markdown opener, and what its
// closer must be. n==3 is bold carrying italic, which WhatsApp expresses as
// bold marks around italic marks.
func emphasisMark(ch byte, n int) (open, close string) {
	if ch == '_' {
		return "*", "*"
	}
	switch n {
	case 1:
		return "_", "_"
	case 3:
		return "*_", "_*"
	default:
		return "*", "*"
	}
}

// oddSingleStars reports whether content closes over an unmatched single
// asterisk, meaning one that opens a span whose close is outside it.
func oddSingleStars(content string) bool {
	count := 0
	for i := 0; i < len(content); i++ {
		if content[i] != '*' {
			continue
		}
		if i > 0 && content[i-1] == '*' {
			continue
		}
		n := runLen(content, i, '*')
		if n%2 == 1 {
			count += n
		}
		i += n - 1
	}
	return count%2 == 1
}

// runLen is the length of the run of b starting at i.
func runLen(s string, i int, b byte) int {
	start := i
	for i < len(s) && s[i] == b {
		i++
	}
	return i - start
}

// nextRun finds the first position at or after from holding a run of at least
// n copies of b, and reports -1 when there is none.
func nextRun(s string, from int, b byte, n int) int {
	for i := from; i < len(s); i++ {
		if s[i] != b {
			continue
		}
		if runLen(s, i, b) >= n {
			return i
		}
	}
	return -1
}

// trimEmphasis strips outer emphasis marks from a cell. The vertical table
// renderer wraps its own label in bold, and wrapping a label that already
// carries WhatsApp's bold would show the stars rather than the word.
func trimEmphasis(s string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "*_~"))
}
