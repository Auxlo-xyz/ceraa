package telegram

import (
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

// An input total built from message usage believes every number the session ever
// stored, including the ones reconciliation had already thrown away.
//
// This is the live shape that produced it: a provider reporting input as a
// running total, four of six runs rejected, and a naive sum of 4,111,306 tokens
// for a session that actually sent 66,107. The report called that figure what
// the session cost.
func TestSessionInputTokensMeasuresWhereTheProviderWasRejected(t *testing.T) {
	sess := &sessionstore.Session{Runs: []sessionstore.Run{
		{Usage: llm.UsageInfo{InputTokens: 4809}, UsageTrust: llm.UsageTrust{Trusted: true, LocalInput: 4622}},
		{Usage: llm.UsageInfo{InputTokens: 9897}, UsageTrust: llm.UsageTrust{Trusted: true, LocalInput: 10438}},
		{Usage: llm.UsageInfo{InputTokens: 2268573}, UsageTrust: llm.UsageTrust{Trusted: false, LocalInput: 23390}},
		{Usage: llm.UsageInfo{InputTokens: 863861}, UsageTrust: llm.UsageTrust{Trusted: false, LocalInput: 9103}},
		{Usage: llm.UsageInfo{InputTokens: 595140}, UsageTrust: llm.UsageTrust{Trusted: false, LocalInput: 9233}},
		{Usage: llm.UsageInfo{InputTokens: 369026}, UsageTrust: llm.UsageTrust{Trusted: false, LocalInput: 9321}},
	}}
	total, measured := sessionInputTokens(sess)
	// Trusted runs still contribute what the provider said, because that is the
	// billing figure and we have no reason to prefer our own count over it.
	want := 4809 + 9897 + 23390 + 9103 + 9233 + 9321
	if total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	if measured != 4 {
		t.Errorf("measured = %d, want 4 rejected runs", measured)
	}
	// The whole point: the naive sum is 62x this.
	if total > 100_000 {
		t.Errorf("total = %d, which is the inflated figure this function exists to avoid", total)
	}
}

// A session with no runs predates run accounting, so messages are the only
// source there is. Absence of runs is not evidence of a bad provider.
func TestSessionInputTokensFallsBackToMessagesWithoutRuns(t *testing.T) {
	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Usage: llm.UsageInfo{InputTokens: 100}},
		{Usage: llm.UsageInfo{InputTokens: 250}},
	}}
	total, measured := sessionInputTokens(sess)
	if total != 350 {
		t.Errorf("total = %d, want 350", total)
	}
	if measured != 0 {
		t.Errorf("measured = %d, want 0 for a session with no runs", measured)
	}
}

// A run stored before the trust field existed deserializes to Trusted: false
// with no measurement. Treating that as rejected would replace a real reported
// figure with zero, which is a worse number than the one it replaced.
func TestSessionInputTokensKeepsARunThatWasNeverMeasured(t *testing.T) {
	sess := &sessionstore.Session{Runs: []sessionstore.Run{
		{Usage: llm.UsageInfo{InputTokens: 5000}, UsageTrust: llm.UsageTrust{}},
	}}
	total, measured := sessionInputTokens(sess)
	if total != 5000 {
		t.Errorf("total = %d, want the 5000 it reported", total)
	}
	if measured != 0 {
		t.Errorf("measured = %d, want 0; a missing verdict is not a rejected one", measured)
	}
}

func TestSessionInputTokensOfNothingIsZero(t *testing.T) {
	if total, measured := sessionInputTokens(nil); total != 0 || measured != 0 {
		t.Errorf("nil session gave total %d, measured %d; want 0, 0", total, measured)
	}
}
