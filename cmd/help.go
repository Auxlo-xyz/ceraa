package cmd

import (
	"fmt"
	"strings"

	"ceraa/internal/tui"
	"github.com/spf13/cobra"
)

// helpWidth is the width help text is laid out at: the terminal's, or 80 when
// the output is piped somewhere with no window to measure.
func helpWidth() int {
	if w := tui.Plain().Width(); w >= 20 {
		return w
	}
	return 80
}

// renderHelp lays a command's help out at width so no line is left for the
// terminal to finish.
//
// Cobra's default template pads the command names and prints every description
// verbatim, which is fine until the description runs past the edge. A terminal
// does not wrap prose, it cuts cells: whatever passes column forty ends up on
// the next row starting at column zero, in the middle of a word. On a phone
// that turned "in the terminal" into "termi" and "nal", which reads as a broken
// build rather than a narrow screen. Every section here is wrapped first, so
// what the terminal receives already ends where the terminal ends.
func renderHelp(cmd *cobra.Command, width int) string {
	if width < 20 {
		width = 20
	}
	var b strings.Builder
	if short := strings.TrimSpace(cmd.Short); short != "" {
		for _, l := range tui.WrapText(short, width) {
			b.WriteString(l + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Usage:\n")
	for _, l := range tui.WrapText(strings.TrimSpace(cmd.UseLine()), width-2) {
		b.WriteString("  " + l + "\n")
	}
	if cmd.HasAvailableSubCommands() {
		b.WriteString("  " + cmd.CommandPath() + " [command]\n")
	}
	if cmds := availableHelpCommands(cmd); len(cmds) > 0 {
		b.WriteString("\nAvailable Commands:\n")
		pad := 0
		for _, c := range cmds {
			if n := len(c.Name()); n > pad {
				pad = n
			}
		}
		pad += 2
		for _, c := range cmds {
			for _, l := range helpCommandRow(c.Name(), c.Short, pad, width) {
				b.WriteString(l + "\n")
			}
		}
	}
	if long := strings.TrimSpace(cmd.Long); long != "" {
		b.WriteString("\n")
		// Line breaks are kept, not reflowed: a Long carries its own tables,
		// and reflowing one would trade the terminal's mid-word cut for ours.
		// Only lines longer than the width are touched, and only at spaces.
		for _, l := range tui.WrapText(long, width) {
			b.WriteString(l + "\n")
		}
	}
	if cmd.HasAvailableLocalFlags() {
		b.WriteString("\nFlags:\n")
		b.WriteString(strings.TrimRight(cmd.LocalFlags().FlagUsagesWrapped(width), "\n") + "\n")
	}
	if cmd.HasAvailableSubCommands() {
		b.WriteString("\n")
		footer := fmt.Sprintf("Use %q for more information about a command.", cmd.CommandPath()+" [command] --help")
		for _, l := range tui.WrapText(footer, width) {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// availableHelpCommands is the same list cobra's default template shows: every
// command that is not hidden or deprecated, in cobra's sorted order.
func availableHelpCommands(cmd *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, c := range cmd.Commands() {
		if c.IsAvailableCommand() {
			out = append(out, c)
		}
	}
	return out
}

// helpCommandRow renders one row of the command table: the name padded into its
// column and the description wrapped beside it, hanging under its own start
// column rather than the row's.
//
// When the names leave fewer than a dozen cells for the description, honesty
// beats alignment and the description goes underneath on its own lines. Squeezing
// it beside the name would wrap every other word and read worse than the break.
func helpCommandRow(name, short string, pad, width int) []string {
	head := "  " + name
	if strings.TrimSpace(short) == "" {
		return []string{head}
	}
	col := 2 + pad
	if width-col < 12 {
		out := []string{head}
		for _, l := range tui.WrapText(short, width-4) {
			out = append(out, "    "+l)
		}
		return out
	}
	head += strings.Repeat(" ", col-len(head))
	lines := tui.WrapText(short, width-col)
	out := []string{head + lines[0]}
	hang := strings.Repeat(" ", col)
	for _, l := range lines[1:] {
		out = append(out, hang+l)
	}
	return out
}

// useWrappedHelp points a command at the width-aware renderer. The width is read
// when help is asked for, not when the command is registered, so a resized
// terminal is measured fresh.
func useWrappedHelp(cmd *cobra.Command) {
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), renderHelp(c, helpWidth()))
	})
}
