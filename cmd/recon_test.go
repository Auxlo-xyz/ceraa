package cmd

// recon_test.go covers the operator surface of the recon engagement: the
// record it writes, the way it reads back, and the way it clears.
//
// The harness side of the same record (the gate, the budget, the
// destructive-method rule) is tested in the recon package. What matters
// here is that the CLI and the harness agree on one file.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/recon"
)

func TestReconEngageWritesTheRecordTheHarnessReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CERAA_HOME", home)
	recon.Stop()

	out := &bytes.Buffer{}
	err := runReconEngage(out, reconOptions{
		Target:     "https://example.com",
		InScope:    []string{"example.com"},
		OutScope:   []string{"admin.example.com"},
		UserAgent:  "ceraa-engagement/1.0",
		RPM:        30,
		RequestCap: 200,
		Objective:  "surface review",
	})
	if err != nil {
		t.Fatalf("engage failed: %v", err)
	}
	if !strings.Contains(out.String(), "engagement recorded") {
		t.Errorf("engage did not confirm:\n%s", out)
	}
	if !strings.Contains(out.String(), "ceraa-engagement/1.0") {
		t.Errorf("engage did not echo the user agent:\n%s", out)
	}

	record := filepath.Join(home, "recon-scope.json")
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("no record written: %v", err)
	}
	engagement, ok := recon.Active()
	if !ok {
		t.Fatal("the harness does not read the record back")
	}
	if engagement.Target != "https://example.com" {
		t.Errorf("target = %q", engagement.Target)
	}
	if engagement.UserAgent != "ceraa-engagement/1.0" {
		t.Errorf("user agent = %q", engagement.UserAgent)
	}
	if engagement.RPM != 30 || engagement.RequestCap != 200 {
		t.Errorf("budgets = %d rpm, %d cap", engagement.RPM, engagement.RequestCap)
	}
	if len(engagement.OutScope) != 1 || engagement.OutScope[0] != "admin.example.com" {
		t.Errorf("out of scope = %v", engagement.OutScope)
	}
	if engagement.AllowDestructive {
		t.Error("destructive requests are authorized without the flag")
	}
}

func TestReconEngageRefusesAnEmptyEngagement(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	recon.Stop()

	out := &bytes.Buffer{}
	if err := runReconEngage(out, reconOptions{}); err == nil {
		t.Fatal("an engagement with neither target nor scope tokens was accepted")
	}
	if _, ok := recon.Active(); ok {
		t.Error("a refused engagement left a record behind")
	}
}

func TestReconEngageRefusesNegativeBudgets(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	recon.Stop()

	out := &bytes.Buffer{}
	if err := runReconEngage(out, reconOptions{Target: "https://example.com", RPM: -1}); err == nil {
		t.Fatal("a negative rate limit was accepted")
	}
}

func TestReconEngageCarriesDestructiveAuthorization(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	recon.Stop()

	out := &bytes.Buffer{}
	if err := runReconEngage(out, reconOptions{Target: "https://example.com", AllowDestructive: true}); err != nil {
		t.Fatalf("engage failed: %v", err)
	}
	engagement, ok := recon.Active()
	if !ok || !engagement.AllowDestructive {
		t.Fatalf("destructive authorization did not stick: %+v", engagement)
	}
	if !strings.Contains(out.String(), "PUT, PATCH and DELETE are authorized") {
		t.Errorf("engage did not report the destructive rule:\n%s", out)
	}
}

func TestReconStatusAndStop(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	recon.Stop()

	engageOut := &bytes.Buffer{}
	if err := runReconEngage(engageOut, reconOptions{Target: "https://example.com"}); err != nil {
		t.Fatalf("engage failed: %v", err)
	}

	statusOut := &bytes.Buffer{}
	if err := runReconStatus(statusOut); err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if !strings.Contains(statusOut.String(), "https://example.com") {
		t.Errorf("status did not show the target:\n%s", statusOut)
	}

	stopOut := &bytes.Buffer{}
	if err := runReconStop(stopOut); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	if _, ok := recon.Active(); ok {
		t.Error("the engagement survived stop")
	}
}

func TestReconStatusReportsNoEngagement(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	recon.Stop()

	out := &bytes.Buffer{}
	if err := runReconStatus(out); err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if !strings.Contains(out.String(), "no engagement is active") {
		t.Errorf("status did not report the empty state:\n%s", out)
	}
}

func TestReconCommandIsRegistered(t *testing.T) {
	registerSubcommands()
	found := false
	for _, command := range root.Commands() {
		if command.Name() == "recon" {
			found = true
			for _, sub := range command.Commands() {
				switch sub.Name() {
				case "engage", "status", "stop":
				default:
					t.Errorf("unexpected recon subcommand %q", sub.Name())
				}
			}
		}
	}
	if !found {
		t.Fatal("the recon command is not registered")
	}
}
