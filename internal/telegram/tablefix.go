package telegram

import "strings"

// Table healing for rich markdown.
//
// The model's tables reach Telegram's server-side parser in the shape the
// model actually wrote, and the parser follows the rich-markdown dialect:
// header row first, a delimiter row under it, then body rows. Two habits from
// live transcripts break that shape: the delimiter row emitted ABOVE the
// header, and the delimiter row missing entirely. Nothing here asks the agent
// to write better tables: fixTables rewrites the message at the send choke
// into the shape the dialect expects, and passes anything else through
// untouched.

// fixTables rewrites malformed pipe tables in md into the shape Telegram's
// rich markdown parses, leaving every other byte of the message alone.
// Code fences are skipped so a table quoted inside one is never touched.
func fixTables(md string) string {
	if !strings.Contains(md, "|") {
		return md
	}
	lines := strings.Split(md, "\n")
	var out []string
	inFence := false
	healed := false
	i := 0
	for i < len(lines) {
		line := lines[i]
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			out = append(out, line)
			i++
			continue
		}
		if inFence || !isTableLine(line) {
			out = append(out, line)
			i++
			continue
		}
		end := i
		for end < len(lines) && isTableLine(lines[end]) {
			end++
		}
		rows := append([]string(nil), lines[i:end]...)
		if fixed := healTable(rows); len(fixed) != len(rows) || !equalRows(rows, fixed) {
			healed = true
			out = append(out, fixed...)
		} else {
			out = append(out, rows...)
		}
		i = end
	}
	if !healed {
		return md
	}
	return strings.Join(out, "\n")
}

func equalRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// isTableLine reports whether a line opens or continues a pipe table. A
// line that merely mentions a pipe mid-sentence does not qualify: the mark is
// the line being led by a pipe, which is how every table row on the wire
// begins.
func isTableLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "|")
}

// isDelimiterRow reports whether the row is a GFM delimiter row: every cell
// is dashes with optional alignment colons. A single-cell row is not a
// delimiter of anything, and at least two cells are required so a shorthand
// bullet like `| ---` inside prose never counts.
func isDelimiterRow(line string) bool {
	cells := rowCells(line)
	if len(cells) < 2 {
		return false
	}
	for _, cell := range cells {
		t := strings.TrimSpace(cell)
		t = strings.TrimPrefix(t, ":")
		t = strings.TrimSuffix(t, ":")
		if t == "" || strings.Trim(t, "-") != "" {
			return false
		}
	}
	return true
}

// rowCells splits a table row into its cells. The split is raw: escaped pipes
// inside cell text come back as separate cells on the way in, and rows are
// always rebuilt with the same delimiter, so a rebuild round-trips the row
// byte for byte.
func rowCells(line string) []string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")
	if strings.TrimSpace(trimmed) == "" {
		return nil
	}
	return strings.Split(trimmed, "|")
}

// delimiterRow returns a delimiter row with n cells.
func delimiterRow(n int) string {
	cells := make([]string, n)
	for i := range cells {
		cells[i] = "---"
	}
	return rebuildRow(cells)
}

// delimiterOfCells resizes an existing delimiter row to the header's column
// count. Cells beyond the header's width are dropped; missing ones are filled
// with plain dashes. Every kept cell keeps its own alignment markers, so a
// live `|:---|:---:|` survives a resize byte for byte.
func delimiterOfCells(cells []string, want int) string {
	for len(cells) < want {
		cells = append(cells, "---")
	}
	return rebuildRow(cells[:want])
}

// rebuildRow assembles a table row from raw cell fragments. The fragments are
// kept exactly as they were split off the row, so nothing inside a cell is
// reformatted and only the pipes that join them are re-laid.
func rebuildRow(cells []string) string {
	return "|" + strings.Join(cells, "|") + "|"
}

// healTable returns a healed copy of one table block: the header-delimiter
// order corrected, a delimiter inserted when the block has none, and short
// body rows padded under the header's column count. Content is preserved:
// rows are rebuilt with the same raw delimiter they were split on.
func healTable(rows []string) []string {
	out := append([]string(nil), rows...)
	// Fault one: the delimiter row above the header. A block that opens with
	// a delimiter row followed by a normal row has the two in the wrong
	// order: the parser reads the dashes as header text and then shifts every
	// column by one.
	if len(out) >= 2 && isDelimiterRow(out[0]) && !isDelimiterRow(out[1]) {
		out[0], out[1] = out[1], out[0]
	}
	// Fault two: no delimiter row at all. The header is row zero and the
	// following rows read as literal pipe text, so a delimiter matching the
	// header's column count is inserted under it.
	wantsTable := len(out) >= 2
	if wantsTable && !anyDelimiterRow(out) {
		cells := rowCells(out[0])
		fixed := make([]string, 0, len(out)+1)
		fixed = append(fixed, out[0], delimiterRow(len(cells)))
		fixed = append(fixed, out[1:]...)
		out = fixed
	}
	// Fault three: a delimiter whose column count disagrees with the header.
	// A wider delimiter opens phantom columns and shifts every row off its
	// header; a narrower one loses the last columns. The delimiter is trimmed
	// or grown in place to the header's width, keeping each remaining cell's
	// own alignment markers. This is the live transcript fault of Oct 21: a
	// three-cell delimiter under a two-column header.
	if len(out) >= 2 && isDelimiterRow(out[1]) {
		if cells := rowCells(out[1]); len(cells) != len(rowCells(out[0])) {
			out[1] = delimiterOfCells(cells, len(rowCells(out[0])))
		}
	}
	// Fault three: ragged body rows. A row shorter than the header decouples
	// the parser's column model, so short rows are padded with empty cells in
	// place. Rows longer than the header are left alone: joining spill cells
	// risks eating content, and a wide row still renders. The padding runs
	// only once the block is confirmed to be a table, and body rows start
	// after the header and its delimiter.
	if wantsTable {
		want := len(rowCells(out[0]))
		for i := 2; i < len(out); i++ {
			if isDelimiterRow(out[i]) {
				continue
			}
			if have := len(rowCells(out[i])); have < want {
				out[i] = padRow(out[i], want-have)
			}
		}
	}
	return out
}

func anyDelimiterRow(rows []string) bool {
	for _, row := range rows {
		if isDelimiterRow(row) {
			return true
		}
	}
	return false
}

// padRow appends n empty cells to a row, keeping whatever trailing pipe and
// spacing it already had.
func padRow(line string, n int) string {
	row := strings.TrimSpace(line)
	if !strings.HasSuffix(row, "|") {
		row += " |"
	}
	for range n {
		row += " |"
	}
	return row
}
