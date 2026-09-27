package telegram

import (
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

func trustedRun(host, model string, input, cached int) sessionstore.Run {
	return sessionstore.Run{
		ProviderHost: host,
		PricingModel: model,
		Usage:        llm.UsageInfo{InputTokens: input, CachedInputTokens: cached},
		UsageTrust:   llm.UsageTrust{Trusted: true},
	}
}

func rejectedRun(host, model string, input, cached int) sessionstore.Run {
	return sessionstore.Run{
		ProviderHost: host,
		PricingModel: model,
		Usage:        llm.UsageInfo{InputTokens: input, CachedInputTokens: cached},
		UsageTrust:   llm.UsageTrust{Trusted: false, Note: "running total"},
	}
}

// Averaging the ratio per run would let a 20K-token run count as much as a 2M one.
// The totals have to be weighted by tokens, which is the only fair comparison.
func TestAggregateCacheIsTokenWeighted(t *testing.T) {
	runs := []sessionstore.Run{
		trustedRun("https://a.example", "m", 1_000_000, 900_000), // 90%
		trustedRun("https://a.example", "m", 10_000, 0),          // 0%
	}
	stats := aggregateCache(runs)
	if len(stats) != 1 {
		t.Fatalf("got %d stats, want 1", len(stats))
	}
	want := 900_000.0 / 1_010_000
	if diff := stats[0].HitRate() - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("HitRate = %v, want %v (a per-run average would say 45%%)", stats[0].HitRate(), want)
	}
}

// A route whose counts were rejected must not be presented as a route that cached
// nothing. Those are different answers, and conflating them is how a broken route
// reads as a working one.
func TestRejectedRunsAreNotCountedAsZeroCache(t *testing.T) {
	runs := []sessionstore.Run{
		rejectedRun("https://bad.example", "m", 50_000_000, 49_000_000),
	}
	stats := aggregateCache(runs)
	if len(stats) != 1 {
		t.Fatalf("got %d stats, want 1", len(stats))
	}
	if stats[0].Trusted != 0 {
		t.Error("a rejected run was counted as trusted")
	}
	if stats[0].Rejected != 1 {
		t.Errorf("Rejected = %d, want 1", stats[0].Rejected)
	}
	if stats[0].HitRate() != 0 {
		t.Errorf("HitRate = %v; a rejected route must not assert a rate", stats[0].HitRate())
	}
	if stats[0].Input != 0 {
		t.Errorf("a rejected run contributed %d input tokens to the totals", stats[0].Input)
	}
}

// Endpoints are kept apart, because a session mixes routes and an aggregate hides
// which one is at fault.
func TestAggregateCacheSeparatesEndpoints(t *testing.T) {
	runs := []sessionstore.Run{
		trustedRun("https://good.example", "m", 1000, 990),
		trustedRun("https://other.example", "m", 1000, 100),
	}
	stats := aggregateCache(runs)
	if len(stats) != 2 {
		t.Fatalf("got %d stats, want 2", len(stats))
	}
	if stats[0].Endpoint != "https://good.example" {
		t.Errorf("best-caching endpoint should sort first, got %q", stats[0].Endpoint)
	}
}

// Runs recorded before ProviderHost existed must not vanish from the report.
func TestAggregateCacheSurvivesUnrecordedEndpoints(t *testing.T) {
	runs := []sessionstore.Run{
		{Usage: llm.UsageInfo{InputTokens: 100, CachedInputTokens: 50}, UsageTrust: llm.UsageTrust{Trusted: true}},
	}
	stats := aggregateCache(runs)
	if len(stats) != 1 || stats[0].Endpoint != "(unrecorded)" {
		t.Fatalf("unrecorded endpoint lost: %+v", stats)
	}
}

// A base URL can carry credentials in its userinfo, and this text goes into a chat
// message, so it is stripped.
func TestShortHostStripsSchemeAndCredentials(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com":              "api.example.com",
		"http://api.example.com/v1":            "api.example.com",
		"https://user:secret@api.example.com":  "api.example.com",
		"https://api.example.com?key=abcd1234": "api.example.com",
	}
	for in, want := range cases {
		if got := shortHost(in); got != want {
			t.Errorf("shortHost(%q) = %q, want %q", in, got, want)
		}
	}
	if strings.Contains(shortHost("https://tok:tok@api.example.com"), "tok") {
		t.Error("credentials survived into the rendered host")
	}
}

// The report has to distinguish "measured and low" from "not measured", or a
// broken route reads as a healthy one.
func TestCacheReportDistinguishesUnknownFromLow(t *testing.T) {
	sess := &sessionstore.Session{Runs: []sessionstore.Run{
		trustedRun("https://good.example", "m", 100000, 99000),
		rejectedRun("https://bad.example", "m", 50_000_000, 0),
	}}
	out := cacheReport(sess)
	if !strings.Contains(out, "99.0%") {
		t.Errorf("measured rate missing: %s", out)
	}
	if !strings.Contains(out, "unknown") {
		t.Errorf("the unmeasured route is not marked unknown: %s", out)
	}
	if !strings.Contains(out, "1 of 2 runs measured") {
		t.Errorf("the measured/rejected split is missing: %s", out)
	}
}
