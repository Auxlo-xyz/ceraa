package llm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// TestLiveCodexCacheAffinity proves the transport earns a prompt-cache hit
// against the real endpoint.
//
// The unit tests pin the two things that make this work: the request carries a
// session header, and the cache key fits the length the provider accepts. Both
// can pass while the combination buys nothing, because a silently ignored field
// is the failure mode here. An earlier version of this client sent neither and
// still returned 200 on every request; only the reported cache numbers showed
// that roughly half of a perfect prefix was being re-read.
//
// So this asserts on the provider's own numbers. It sends one long fixed prefix
// repeatedly with a single cache key and requires every repeat after the first
// to come back warm. Measured behaviour on this endpoint:
//
//	with the session header    5 of 5, and 12 of 12, and 7 of 7 warm
//	without it                1-2 of 5 warm, the rest cold
//
// A cache that is working reports a warm prefix on every repeat. One that is not
// reports roughly coin-flips. That difference is what the test turns into a
// failure.
//
// Cost is deliberately small: one ~3.5k token prefix, repeated five times.
// Skipped unless CERAA_LIVE_CODEX=1 and CERAA_HOME is set, so it never runs in
// CI and never runs by accident.
func TestLiveCodexCacheAffinity(t *testing.T) {
	if os.Getenv("CERAA_LIVE_CODEX") != "1" {
		t.Skip("set CERAA_LIVE_CODEX=1 to run the live cache affinity check")
	}
	dir := os.Getenv("CERAA_HOME")
	if dir == "" {
		t.Fatal("CERAA_HOME must be set; refusing to run")
	}
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}

	// Comfortably past any activation minimum, so a cold read cannot be
	// explained by a prefix too small to cache, and small enough that five
	// repeats cost almost nothing.
	//
	// Sized from measurement, not from the published minimum. A prefix that
	// tokenised to 1,229 tokens - comfortably over the documented 1,024 - came
	// back cold on all five repeats, while one tokenising to 3,433 read 2,816
	// warm. The documented floor is not what this endpoint actually honours, so
	// the fixture clears it by a wide margin rather than sitting on it.
	const filler = "The quick brown fox jumps over the lazy dog. "
	prefix := "You are a cache affinity probe. Follow instructions literally. " + strings.Repeat(filler, 320)
	messages := []Item{
		{Kind: "text", Role: RoleSystem, Text: prefix},
		{Kind: "text", Role: RoleUser, Text: "Reply with exactly one word: OK"},
	}

	client := New(ProviderConfig{
		BaseURL:     CodexBaseURL,
		Model:       DefaultCodexModel,
		CodexNative: true,
		CodexTokens: liveCodexTokens{},
		MaxRetries:  1,
		Timeout:     120 * time.Second,
	})

	// One key for the whole run. If the transport ever derived the key from
	// anything volatile, the repeats would stop sharing a cache and this fails
	// for a different reason, which the log below makes obvious.
	cacheKey := clampPromptCacheKey("ceraa-live-cache-affinity")
	if len(cacheKey) > promptCacheKeyMaxLen {
		t.Fatalf("cache key is %d chars, over the %d the provider accepts", len(cacheKey), promptCacheKeyMaxLen)
	}

	const repeats = 5
	type reading struct {
		input  int
		cached int
	}
	var got []reading

	for i := 1; i <= repeats; i++ {
		var usage *UsageInfo
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		err := client.Stream(ctx, StreamRequest{
			Messages:       messages,
			PromptCacheKey: cacheKey,
			Reason:         ReasonLow,
		}, func(r ProviderResponse) {
			if r.Done {
				usage = r.Usage
			}
		})
		cancel()
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if usage == nil {
			t.Fatalf("repeat %d produced no usage", i)
		}
		got = append(got, reading{usage.InputTokens, usage.CachedInputTokens})
		pct := 0.0
		if usage.InputTokens > 0 {
			pct = float64(usage.CachedInputTokens) / float64(usage.InputTokens) * 100
		}
		t.Logf("repeat %d: input=%d cached=%d (%.1f%%)", i, usage.InputTokens, usage.CachedInputTokens, pct)
		// Space the calls the way a real session spaces them. Back-to-back
		// requests can share a replica that has not finished settling, which
		// would make this test measure the endpoint's write path instead of
		// the client's affinity.
		if i < repeats {
			time.Sleep(5 * time.Second)
		}
	}

	// The first request writes the cache, so a cold read there is correct
	// behaviour rather than a defect.
	var misses []int
	for i, r := range got[1:] {
		if r.cached == 0 {
			misses = append(misses, i+2)
		}
	}
	if len(misses) > 0 {
		var b strings.Builder
		for i, r := range got {
			fmt.Fprintf(&b, "\n  repeat %d: input=%d cached=%d", i+1, r.input, r.cached)
		}
		t.Errorf("repeats %v of an identical prefix came back cold; a working session header makes them all warm%s",
			misses, b.String())
		return
	}

	totalCached, totalInput := 0, 0
	for _, r := range got[1:] {
		totalCached += r.cached
		totalInput += r.input
	}
	t.Logf("LIVE CACHE AFFINITY OK: %d of %d repeats warm, %.1f%% of prefix reused",
		len(got)-1, repeats, float64(totalCached)/float64(totalInput)*100)
}
