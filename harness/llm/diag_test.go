package llm

import (
	"os"
	"strings"
	"testing"
)

// Every diagnostic is written twice: once to the log file, which is the durable
// copy, and once to a live stream, which is only for whoever is watching. The
// live stream is swapped here so a command that owns the terminal can take its
// own stderr back.
func TestSetDiagOutputSwapsTheLiveDestination(t *testing.T) {
	t.Cleanup(func() { SetDiagOutput(os.Stderr) })

	var b strings.Builder
	SetDiagOutput(&b)
	diagNote("first\n")
	if got := b.String(); !strings.Contains(got, "ceraa: first") {
		t.Errorf("diagnostic did not reach the writer: %q", got)
	}

	// A nil writer silences rather than panicking: the caller is saying "I have
	// somewhere else for these", not "send them into a nil interface".
	SetDiagOutput(nil)
	diagNote("second\n")
	if got := b.String(); strings.Contains(got, "second") {
		t.Errorf("a nil destination did not silence the note: %q", got)
	}
}
