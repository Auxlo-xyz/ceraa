package cmd

// recon.go is the operator's side of the recon layer.
//
// The scope gate, the rate budget and the destructive-method rule are all
// enforced in the harness, but nothing in the harness could ever produce an
// engagement on its own: the model must not be able to widen its own
// authorization. So the engagement is set from outside, by the operator, and
// it lives on disk where the running service reads it.
//
// The record is written by this process and consumed by the daemon, which is
// why the write is the contract and not an in-memory field. `recon stop`
// deletes it, and a daemon that has no engagement in memory reads the file
// afresh on every gate check, so stopping it here really stops it there.

import (
	"fmt"
	"io"
	"strings"

	"ceraa/harness/recon"
	"github.com/spf13/cobra"
)

// reconOptions is one engagement as the operator asked for it. The command
// fills it from flags; the functions below work on the struct so they can be
// tested without going through cobra.
type reconOptions struct {
	Target           string
	Objective        string
	InScope          []string
	OutScope         []string
	UserAgent        string
	RPM              int
	RequestCap       int
	AllowDestructive bool
	// AllowPrivate names the private ranges the engagement may reach. The
	// egress wall refuses them by default because they are this host's own
	// network, and an internal engagement lives there.
	AllowPrivate []string
}

var reconFlags reconOptions

var reconCmd = &cobra.Command{
	Use:   "recon",
	Short: "Manage the recon engagement: target, scope, budgets, user agent",
	Long: "The engagement is the authorization layer under the probe and the secret scanner.\n" +
		"It names what may be probed, what must never be probed, how fast, how many times,\n" +
		"and which user agent every request carries. The model cannot change any of it.",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var reconEngageCmd = &cobra.Command{
	Use:   "engage",
	Short: "Set the active engagement",
	Long: "Record an engagement on disk. A running service picks it up on its next\n" +
		"request. One engagement is active at a time: a new one replaces the old.",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReconEngage(cmd.OutOrStdout(), reconFlags)
	},
}

var reconStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the active engagement",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReconStatus(cmd.OutOrStdout())
	},
}

var reconStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Clear the active engagement",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReconStop(cmd.OutOrStdout())
	},
}

// runReconEngage records the engagement. An engagement with neither a target
// nor a scope token authorizes nothing, so it is refused rather than stored
// as a record that reads as permission.
func runReconEngage(out io.Writer, options reconOptions) error {
	target := strings.TrimSpace(options.Target)
	inScope := cleanTokens(options.InScope)
	outScope := cleanTokens(options.OutScope)
	if target == "" && len(inScope) == 0 {
		return fmt.Errorf("engage needs --target or at least one --in-scope token")
	}
	if options.RPM < 0 || options.RequestCap < 0 {
		return fmt.Errorf("--rpm and --request-cap must not be negative")
	}
	engagement := recon.Engagement{
		Target:           target,
		Objective:        strings.TrimSpace(options.Objective),
		InScope:          inScope,
		OutScope:         outScope,
		UserAgent:        strings.TrimSpace(options.UserAgent),
		RPM:              options.RPM,
		RequestCap:       options.RequestCap,
		AllowDestructive: options.AllowDestructive,
		AllowPrivate:     cleanTokens(options.AllowPrivate),
	}
	if err := recon.SetActive(engagement); err != nil {
		return err
	}
	active, ok := recon.Active()
	if !ok {
		return fmt.Errorf("engagement did not stick")
	}
	fmt.Fprintln(out, "engagement recorded")
	printEngagement(out, active)
	return nil
}

// runReconStatus prints the engagement the harness would apply right now.
func runReconStatus(out io.Writer) error {
	active, ok := recon.Active()
	if !ok {
		fmt.Fprintln(out, "no engagement is active")
		return nil
	}
	printEngagement(out, active)
	return nil
}

// runReconStop clears the engagement and deletes the record.
func runReconStop(out io.Writer) error {
	recon.Stop()
	fmt.Fprintln(out, "engagement cleared")
	return nil
}

// cleanTokens trims and drops empty scope tokens, so a trailing space in a
// shell argument cannot become a scope rule that matches nothing.
func cleanTokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token != "" {
			out = append(out, token)
		}
	}
	return out
}

// printEngagement renders the record the way the operator reads it: what is
// authorized, what is refused, what it costs.
func printEngagement(out io.Writer, e recon.Engagement) {
	if e.Target != "" {
		fmt.Fprintf(out, "target:        %s\n", e.Target)
	} else {
		fmt.Fprintln(out, "target:        (scope tokens only)")
	}
	if e.Objective != "" {
		fmt.Fprintf(out, "objective:     %s\n", e.Objective)
	}
	if len(e.InScope) == 0 {
		fmt.Fprintln(out, "in scope:      target and its subdomains")
	} else {
		fmt.Fprintf(out, "in scope:      %s\n", strings.Join(e.InScope, ", "))
	}
	if len(e.OutScope) == 0 {
		fmt.Fprintln(out, "out of scope:  (none)")
	} else {
		fmt.Fprintf(out, "out of scope:  %s\n", strings.Join(e.OutScope, ", "))
	}
	if e.UserAgent != "" {
		fmt.Fprintf(out, "user agent:    %s\n", e.UserAgent)
	} else {
		fmt.Fprintln(out, "user agent:    (not set)")
	}
	if e.RPM > 0 {
		fmt.Fprintf(out, "rate limit:    %d requests per minute\n", e.RPM)
	} else {
		fmt.Fprintln(out, "rate limit:    uncapped")
	}
	if e.RequestCap > 0 {
		fmt.Fprintf(out, "request cap:   %d requests for the engagement\n", e.RequestCap)
	} else {
		fmt.Fprintln(out, "request cap:   uncapped")
	}
	if e.AllowDestructive {
		fmt.Fprintln(out, "destructive:   PUT, PATCH and DELETE are authorized")
	} else {
		fmt.Fprintln(out, "destructive:   PUT, PATCH and DELETE are refused")
	}
	if len(e.AllowPrivate) == 0 {
		fmt.Fprintln(out, "private nets:  refused (the wall blocks them)")
	} else {
		fmt.Fprintf(out, "private nets:  %s\n", strings.Join(e.AllowPrivate, ", "))
	}
}

func init() {
	reconEngageCmd.Flags().StringVar(&reconFlags.Target, "target", "", "engagement base URL, absolute http(s)")
	reconEngageCmd.Flags().StringVar(&reconFlags.Objective, "objective", "", "what the engagement is for, recorded with the receipts")
	reconEngageCmd.Flags().StringArrayVar(&reconFlags.InScope, "in-scope", nil, "allowlist token: host, .suffix, or host:port (repeatable)")
	reconEngageCmd.Flags().StringArrayVar(&reconFlags.OutScope, "out-scope", nil, "denylist token, same shapes (repeatable)")
	reconEngageCmd.Flags().StringVar(&reconFlags.UserAgent, "user-agent", "", "identifier attached to every request of this engagement")
	reconEngageCmd.Flags().IntVar(&reconFlags.RPM, "rpm", 0, "requests per rolling minute, 0 is uncapped")
	reconEngageCmd.Flags().IntVar(&reconFlags.RequestCap, "request-cap", 0, "total requests for the engagement, 0 is uncapped")
	reconEngageCmd.Flags().BoolVar(&reconFlags.AllowDestructive, "allow-destructive", false, "authorize PUT, PATCH and DELETE")
	reconEngageCmd.Flags().StringArrayVar(&reconFlags.AllowPrivate, "allow-private", nil, "private range the engagement may reach: host, address or CIDR (repeatable)")
	reconCmd.AddCommand(reconEngageCmd, reconStatusCmd, reconStopCmd)
}
