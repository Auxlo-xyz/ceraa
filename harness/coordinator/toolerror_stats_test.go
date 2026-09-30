package coordinator

import (
	"strings"
	"testing"
)

// The classification decides what gets acted on, so the boundary between the
// ordinary and the alarming is the whole content of this file.
func TestToolErrorsAreClassifiedIntoBucketsThatMeanDifferentThings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		want    ToolErrorClass
	}{
		// A missing target is the tool working.
		{"missing file", "read /tmp/nope.txt: no such file or directory", ErrExpected},
		{"no such path", "bash: no such file or directory", ErrExpected},
		{"absent argument", "path is required", ErrExpected},
		{"empty old_text", "edit 0 has no old_text", ErrExpected},
		{"unknown tool", "unknown tool missing; use canonical tools: read", ErrExpected},
		{"timeout", "operation timeout", ErrExpected},
		{"provider overload", "503 service unavailable", ErrExpected},
		{"rate limited", "429 too many requests", ErrExpected},

		// Cancelling is neither a tool problem nor a bug.
		{"canceled", "canceled", ErrCanceled},
		{"context canceled", "context canceled", ErrCanceled},

		// Anything unrecognised is a bug until shown otherwise, so a new failure
		// mode surfaces loudly instead of joining the expected pile.
		// A command that ran and reported non-zero is expected, and is by far the
		// most common thing a shell tool reports. Filing it as a bug would fill
		// the unknown bucket and make the rate that should be zero meaningless.
		{"command exited non-zero", "bash: exit status 1", ErrExpected},
		{"empty message", "", ErrUnknown},
		{"a real defect", "reflect.Value.Call using zero Value argument", ErrUnknown},
		{"a typo in our own code", "ceraa: unexpected nil operation spec", ErrUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyToolError(tc.message); got != tc.want {
				t.Errorf("classify(%q) = %q, want %q", tc.message, got, tc.want)
			}
		})
	}
}

// The case a real session hit three times in seventeen bash calls, for a binary
// that is not installed on the host. It is expected in the sense that the shell
// behaved correctly, and the repetition is the signal: it is a hint about the
// prompt, not the shell.
func TestAMissingBinaryIsClassifiedAndStaysVisible(t *testing.T) {
	if got := classifyToolError("bash: line 1: rg: command not found"); got != ErrExpected {
		t.Errorf("class = %q, want %q", got, ErrExpected)
	}
	// Relative to a baseline, because the counters are process-wide and another
	// test in this package will have moved them.
	before := ToolErrors()
	recordToolCall("bash")
	recordToolError("bash", "bash: line 1: rg: command not found")
	stats := ToolErrors()
	if stats.Calls["bash"] != before.Calls["bash"]+1 || stats.Failures["bash"] != before.Failures["bash"]+1 {
		t.Errorf("bash moved from %d/%d to %d calls / %d failures, want +1/+1",
			before.Calls["bash"], before.Failures["bash"], stats.Calls["bash"], stats.Failures["bash"])
	}
	if stats.ByClass[ErrExpected] != before.ByClass[ErrExpected]+1 {
		t.Errorf("expected class went %d -> %d, want +1", before.ByClass[ErrExpected], stats.ByClass[ErrExpected])
	}
	if len(stats.Samples[ErrExpected]) == 0 {
		t.Error("a failure was counted with no sample, so a rate says how often and not what it looks like")
	}
}

// The number to watch is the unknown share. It should be zero, and every
// non-zero reading is a defect with a sample attached.
func TestTheUnknownRateIsReportedSeparately(t *testing.T) {
	before := ToolErrors().ByClass[ErrUnknown]
	recordToolError("read", "some defect nobody anticipated")
	after := ToolErrors()
	_ = after
	if after.ByClass[ErrUnknown] != before+1 {
		t.Errorf("unknown = %d, want %d", after.ByClass[ErrUnknown], before+1)
	}
	if after.UnknownRate() <= 0 {
		t.Error("an unknown-classed failure left the unknown rate at zero")
	}
	// Only this test's own sample. The counters are process-wide, so a sample
	// added by another test in this package is not this test's to assert on.
	found := false
	for _, s := range after.Samples[ErrUnknown] {
		if strings.Contains(s, "read") && strings.Contains(s, "nobody anticipated") {
			found = true
		}
	}
	if !found {
		t.Errorf("no sample names read and carries the message: %v", after.Samples[ErrUnknown])
	}
}

// Samples are bounded, or a chatty failure turns telemetry into a memory leak.
func TestErrorSamplesAreBounded(t *testing.T) {
	for i := 0; i < maxErrorSamples*3; i++ {
		recordToolError("bash", "no such file or directory")
	}
	if got := len(ToolErrors().Samples[ErrExpected]); got > maxErrorSamples {
		t.Errorf("kept %d samples, over the bound of %d", got, maxErrorSamples)
	}
}
