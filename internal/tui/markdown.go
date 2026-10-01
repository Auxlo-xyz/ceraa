package tui

import (
	"strings"
)

// A Markdown table is prose that happens to be arranged in columns, and the two
// need opposite treatment. A prose wrapper breaks on spaces and lets the
// terminal finish the line, which is right for a paragraph and ruinous for a
// table: the row structure is lost at the first wrap, the columns stop lining up
// because a continuation starts at the margin, and the cells are cut mid-word.
// A tool catalogue came back as a wall of pipes and half words, and the pipes
// were the most legible thing on screen.
//
// So a table is laid out rather than wrapped, and it is laid out in one of two
// forms depending on whether the columns genuinely fit.
//
//   - Aligned, when every column can hold its widest cell and the whole thing
//     still fits the terminal. Header bold, values dim, no rule line.
//   - Stacked, when it does not. A two-column table of tool names and their
//     descriptions cannot be a table on a phone: the name column alone eats
//     half the screen and the description gets a column ten cells wide. So the
//     key goes on its own line and its description is indented under it, which
//     is a list that happens to have come from a table.
//
// The stacked form is the one that matters. It is what a phone gets, and a
// phone is where the pipes were unreadable.

// tableBlock is a parsed Markdown table.
type tableBlock struct {
	header []string
	rows   [][]string
}

// parseTableRow splits one pipe-led line into its cells.
//
// A leading or trailing pipe is layout rather than data and is dropped, and so
// is a row with fewer than two cells: a line with one pipe in it is prose that
// happens to mention a pipe, and treating it as a table is worse than waiting
// to see the rule line that follows.
func parseTableRow(line string) ([]string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.Contains(trimmed, "|") {
		return nil, false
	}
	cells := strings.Split(trimmed, "|")
	if len(cells) > 0 && strings.TrimSpace(cells[0]) == "" {
		cells = cells[1:]
	}
	if len(cells) > 0 && strings.TrimSpace(cells[len(cells)-1]) == "" {
		cells = cells[:len(cells)-1]
	}
	if len(cells) < 2 {
		return nil, false
	}
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells, true
}

// isTableRule reports whether cells are the dashes-and-colons rule that tells a
// header from its body.
//
// At least three dashes per cell, which is what GitHub's own parser accepts, so
// a prose line that happens to contain pipes and hyphens is not mistaken for
// the start of a table.
func isTableRule(cells []string) bool {
	if len(cells) < 2 {
		return false
	}
	for _, c := range cells {
		body := strings.Trim(strings.TrimSpace(c), ":")
		if len(body) < 3 {
			return false
		}
		for _, r := range body {
			if r != '-' {
				return false
			}
		}
	}
	return true
}

// stripEmphasis removes the markers a table cell arrives carrying.
//
// Emphasis is removed rather than rendered because a cell is a fragment: bolding
// the tool name and nothing else in a row reads as a mistake, and half a
// rendered run is worse than the plain text. Underscores are left alone on
// purpose. A tool catalogue is full of snake_case names and italics inside a
// table cell are vanishingly rare, so stripping every underscore would mangle
// run_command_with_secret into runcommandwithsecret.
func stripEmphasis(s string) string {
	s = strings.ReplaceAll(s, "`", "")
	for _, marker := range []string{"**", "__", "*"} {
		for {
			start := strings.Index(s, marker)
			if start < 0 {
				break
			}
			rest := s[start+len(marker):]
			end := strings.Index(rest, marker)
			if end < 0 {
				break
			}
			s = s[:start] + rest[:end] + s[start+2*len(marker)+end:]
		}
	}
	return s
}

// renderTable lays out a table at width, choosing the form that reads.
func renderTable(p Palette, t *tableBlock, width int) []string {
	if len(t.header) == 0 {
		return nil
	}
	cols := len(t.header)

	// Normalise first: emphasis off, missing cells present but empty, extra
	// cells dropped. Every later step can then assume a rectangle.
	grid := make([][]string, 0, len(t.rows)+1)
	head := make([]string, cols)
	for i, c := range t.header {
		head[i] = stripEmphasis(c)
	}
	grid = append(grid, head)
	for _, r := range t.rows {
		row := make([]string, cols)
		for i := range cols {
			if i < len(r) {
				row[i] = stripEmphasis(r[i])
			}
		}
		grid = append(grid, row)
	}

	natural := make([]int, cols)
	for _, row := range grid {
		for i, c := range row {
			if w := visibleWidth(c); w > natural[i] {
				natural[i] = w
			}
		}
	}

	const (
		indent = 2
		gutter = 2
	)
	needed := indent
	for i, w := range natural {
		needed += w
		if i > 0 {
			needed += gutter
		}
	}
	if needed <= width {
		return alignedTable(p, grid, natural, indent, gutter)
	}
	return stackedTable(p, grid, indent, gutter, width)
}

// alignedTable keeps the columns, which is the whole point when they fit.
func alignedTable(p Palette, grid [][]string, natural []int, indent, gutter int) []string {
	var out []string
	for ri, row := range grid {
		cells := make([][]string, len(row))
		height := 1
		for i, c := range row {
			cells[i] = wrapText(c, natural[i])
			if len(cells[i]) > height {
				height = len(cells[i])
			}
		}
		for line := 0; line < height; line++ {
			var b strings.Builder
			b.WriteString(strings.Repeat(" ", indent))
			for i := range row {
				if i > 0 {
					b.WriteString(strings.Repeat(" ", gutter))
				}
				part := ""
				if line < len(cells[i]) {
					part = cells[i][line]
				}
				b.WriteString(part)
				if i < len(row)-1 {
					b.WriteString(strings.Repeat(" ", natural[i]-visibleWidth(part)))
				}
			}
			text := strings.TrimRight(b.String(), " ")
			if ri == 0 {
				out = append(out, p.Bold(text))
			} else {
				out = append(out, p.Dim(text))
			}
		}
	}
	return out
}

// stackedTable gives up the columns rather than squeezing them.
func stackedTable(p Palette, grid [][]string, indent, gutter, width int) []string {
	var out []string
	head := grid[0]

	// With more than two columns the header names are the only thing saying what
	// they are, so they are kept as a legend. With two they are obvious from the
	// shape and repeating them is noise.
	if len(head) > 2 {
		out = append(out, p.Dim(strings.TrimRight(
			strings.Repeat(" ", indent)+strings.Join(head, " / "), " ")))
	}

	wrapW := width - indent - gutter
	if wrapW < 8 {
		wrapW = 8
	}
	for i, row := range grid[1:] {
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, strings.Repeat(" ", indent)+row[0])
		for ci := 1; ci < len(row); ci++ {
			if strings.TrimSpace(row[ci]) == "" {
				continue
			}
			body := row[ci]
			if len(head) > 2 {
				body = head[ci] + ": " + body
			}
			for _, l := range wrapText(body, wrapW) {
				out = append(out, strings.Repeat(" ", indent+gutter)+l)
			}
		}
	}
	return out
}
