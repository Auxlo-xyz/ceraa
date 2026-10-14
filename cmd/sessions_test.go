package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/harness/sessionstore"
	"ceraa/internal/config"
)

func TestSessionsCommandIsRegistered(t *testing.T) {
	registerSubcommands()
	for _, command := range root.Commands() {
		if command.Name() == "sessions" {
			return
		}
	}
	t.Fatal("sessions command is not registered")
}

func TestRunSessionsListsRunsReadOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.SessionFile = dir
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	if err := store.Save(&sessionstore.Session{
		ID:         "alpha",
		Title:      "Build report",
		Messages:   []sessionstore.Message{},
		Operations: []sessionstore.Operation{},
		Runs:       []sessionstore.Run{{ID: "run-1", Status: sessionstore.RunCompleted, Step: 3, Requests: 2, CreatedAt: created, UpdatedAt: created}},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "alpha.json"))
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runSessions(&out, ""); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"Session: alpha", "Status: completed", "Title: Build report", "Run: run-1", "Steps: 3", "Requests: 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	after, err := os.ReadFile(filepath.Join(dir, "alpha.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("sessions inspection changed the session file")
	}
}

func TestRunSessionsKeepsEachSessionOnLabeledLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.SessionFile = dir
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&sessionstore.Session{
		ID:       "multiline",
		Title:    "  First line\nsecond line  ",
		Messages: []sessionstore.Message{}, Operations: []sessionstore.Operation{},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runSessions(&out, "multiline"); err != nil {
		t.Fatal(err)
	}
	want := "Session: multiline\nStatus: idle\nTitle: First line second line\nRuns: none\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestRunSessionsRejectsPathLikeSessionID(t *testing.T) {
	for _, id := range []string{"../alpha", "alpha/beta", ".", ".."} {
		var out bytes.Buffer
		if err := runSessions(&out, id); err == nil {
			t.Errorf("runSessions(%q) succeeded", id)
		}
	}
}

func TestRunSessionsCanInspectOneSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.SessionFile = dir
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alpha", "beta"} {
		if err := store.Save(&sessionstore.Session{ID: id, Messages: []sessionstore.Message{}, Operations: []sessionstore.Operation{}}); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := runSessions(&out, "beta"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "alpha") || !strings.Contains(out.String(), "beta") {
		t.Errorf("single-session output = %q", out.String())
	}
}
