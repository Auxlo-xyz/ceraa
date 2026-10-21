package cmd

// report.go is the operator's side of the finding record.
//
// The record is written by the model through the finding tool, and it is read
// here by whoever has to decide whether to disclose it. That reader needs
// more than the confirmed findings: a disclosure that leaves out what was
// ruled out and what is still unproven cannot tell them what was checked, and
// a report that hides the gaps is worse than no report.
//
// So the coverage always describes the whole record, even when the findings
// are filtered down to one closure state.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"ceraa/harness/findings"
	"ceraa/internal/paths"

	"github.com/spf13/cobra"
)

// reportOptions is one report request. The command fills it from flags and
// the runner works on the struct, so the report can be rendered in a test
// without going through cobra.
type reportOptions struct {
	// Format is md or html.
	Format string
	// Out is the file to write, or empty for stdout.
	Out string
	// Status filters the findings by closure state.
	Status string
	// Severity filters the findings by severity.
	Severity string
}

// ReportCommand returns the `ceraa report` command.
//
// It is a constructor rather than a package-level variable so the flags bind
// to a fresh options struct on every call. A package-level command carries
// its flag values in process-wide state, and a test that built one would
// leave those values set for whoever came next.
func ReportCommand() *cobra.Command {
	options := reportOptions{}
	command := &cobra.Command{
		Use:   "report",
		Short: "Render the finding record as a disclosure report",
		Long: "Render the finding record as a disclosure report.\n" +
			"The record holds every finding, the ones that were ruled out and the ones\n" +
			"that are still open. The report carries all three, because a disclosure\n" +
			"that leaves out the gaps cannot tell the reader what was checked.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReport(cmd.OutOrStdout(), options)
		},
	}
	command.Flags().StringVar(&options.Format, "format", "md", "report format: md or html")
	command.Flags().StringVar(&options.Out, "out", "", "write the report to this file instead of stdout, mode 0600")
	command.Flags().StringVar(&options.Status, "status", "", "only report findings with this closure state: confirmed, ruled_out, open_proof_gap")
	command.Flags().StringVar(&options.Severity, "severity", "", "only report findings with this severity: info, low, medium, high, critical")
	return command
}

// runReport renders the record.
func runReport(out io.Writer, options reportOptions) error {
	format := strings.ToLower(strings.TrimSpace(options.Format))
	if format == "" {
		format = "md"
	}
	if format != "md" && format != "html" {
		return fmt.Errorf("report format %q is not one of md, html", options.Format)
	}

	var statusFilter findings.Status
	if raw := strings.TrimSpace(options.Status); raw != "" {
		parsed, err := findings.ParseStatus(raw)
		if err != nil {
			return err
		}
		statusFilter = parsed
	}
	var severityFilter string
	if raw := strings.TrimSpace(options.Severity); raw != "" {
		parsed, err := findings.ParseSeverity(raw)
		if err != nil {
			return err
		}
		severityFilter = parsed
	}

	dir := filepath.Join(paths.DataDir(), "findings")
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			// An empty record is a state, not a failure: the report says
			// there is nothing yet rather than pretending to have found it.
			fmt.Fprintf(out, "No finding record yet (%s).\n", dir)
			return nil
		}
		return fmt.Errorf("inspect the finding record %q: %w", dir, err)
	}
	store, err := findings.Open(dir)
	if err != nil {
		return err
	}
	list, err := store.List()
	if err != nil {
		return fmt.Errorf("read the finding record: %w", err)
	}
	// The coverage is the whole record, not the filtered slice: the counts
	// are what tell the reader that three other things were ruled out.
	coverage, err := store.Coverage()
	if err != nil {
		return fmt.Errorf("read the finding record: %w", err)
	}
	selected := filterFindings(list, statusFilter, severityFilter)

	var body string
	if format == "html" {
		body = findings.RenderHTML(selected, coverage)
	} else {
		body = findings.RenderMarkdown(selected, coverage)
	}

	target := strings.TrimSpace(options.Out)
	if target == "" {
		fmt.Fprint(out, body)
		return nil
	}
	if err := findings.WriteAtomic(target, []byte(body)); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	fmt.Fprintf(out, "report written to %s\n", target)
	return nil
}

// filterFindings keeps the findings the filters name. An empty filter keeps
// everything.
func filterFindings(list []findings.Finding, status findings.Status, severity string) []findings.Finding {
	out := make([]findings.Finding, 0, len(list))
	for _, f := range list {
		if status != "" && f.Status != status {
			continue
		}
		if severity != "" && f.Severity != severity {
			continue
		}
		out = append(out, f)
	}
	return out
}
