package cachewarm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDecideRefusesSmallPrefix(t *testing.T) {
	d := Decide(time.Now(), time.Now(), DefaultTTL, MinPrefixTokens-1, Economics{InputPerMTok: 3, CacheReadPerMTok: 0.3})
	if d.Warm {
		t.Fatal("a prefix under the provider minimum was scheduled")
	}
	if d.Reason != "prefix too small to be worth a request" {
		t.Fatalf("unexpected reason: %s", d.Reason)
	}
}

func TestDecideRefusesParkedSession(t *testing.T) {
	now := time.Now()
	d := Decide(now, now.Add(-MaxWarmAge-time.Minute), DefaultTTL, 50000, Economics{InputPerMTok: 3, CacheReadPerMTok: 0.3})
	if d.Warm {
		t.Fatal("a session idle past the warm horizon was scheduled")
	}
	if d.Reason != "session idle past the warm horizon" {
		t.Fatalf("unexpected reason: %s", d.Reason)
	}
}

func TestDecideRefusesUnprofitableRefresh(t *testing.T) {
	now := time.Now()
	// A prefix at the size floor with a one-cent-per-million delta earns
	// thousandths of a cent per refresh, which is below both floors at once.
	d := Decide(now, now, DefaultTTL, MinPrefixTokens, Economics{InputPerMTok: 0.01, CacheReadPerMTok: 0})
	if d.Warm {
		t.Fatal("a refresh cheaper than its own floor was scheduled")
	}
	if d.Reason != "expected saving below the floor" {
		t.Fatalf("unexpected reason: %s", d.Reason)
	}
}

func TestDecideSchedulesAtRefreshFraction(t *testing.T) {
	now := time.Now()
	d := Decide(now, now, DefaultTTL, 50000, Economics{InputPerMTok: 3, CacheReadPerMTok: 0.3})
	if !d.Warm {
		t.Fatalf("a profitable refresh was refused: %s", d.Reason)
	}
	if want := time.Duration(float64(DefaultTTL) * RefreshFraction); d.Delay != want {
		t.Fatalf("delay = %s, want %s", d.Delay, want)
	}
}

func TestDecideSubtractsIdleTime(t *testing.T) {
	now := time.Now()
	// An hour-long entry refreshed at 90% has 54 minutes on the clock, so a
	// session idle for 20 of them needs a refresh in 34.
	d := Decide(now, now.Add(-20*time.Minute), DefaultExtendedTTL, 50000, Economics{InputPerMTok: 3, CacheReadPerMTok: 0.3})
	if !d.Warm {
		t.Fatalf("a profitable refresh was refused: %s", d.Reason)
	}
	if want := 34 * time.Minute; d.Delay != want {
		t.Fatalf("delay = %s, want %s", d.Delay, want)
	}
}

func TestDecideRefreshesImmediatelyWhenEntryLapsed(t *testing.T) {
	now := time.Now()
	// Idle past 90% of the TTL: the entry is gone, so the refresh is a write
	// rather than a read, and the choice is now or the full rewrite next turn.
	d := Decide(now, now.Add(-6*time.Minute), DefaultTTL, 50000, Economics{InputPerMTok: 3, CacheReadPerMTok: 0.3})
	if !d.Warm {
		t.Fatalf("a lapsed prefix was refused: %s", d.Reason)
	}
	if d.Delay != 0 {
		t.Fatalf("delay = %s, want 0", d.Delay)
	}
}

func TestEconomicsNoSeparateReadIsFree(t *testing.T) {
	// A provider that bills no cache-read rate bills the whole prompt either
	// way, so the saving is zero and the expected value is zero, not negative.
	e := Economics{InputPerMTok: 3, CacheReadPerMTok: 3}
	if got := e.AvoidedRewrite(100000); got != 0 {
		t.Fatalf("AvoidedRewrite = %v, want 0", got)
	}
}

func TestWarmerArmDisarm(t *testing.T) {
	var mu sync.Mutex
	var calls int
	w := New(func(ctx context.Context, signature string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}, nil)
	if w.Armed() {
		t.Fatal("a fresh warmer reports armed")
	}
	w.Arm("sig", time.Hour)
	if !w.Armed() {
		t.Fatal("Arm did not schedule")
	}
	w.Disarm()
	if w.Armed() {
		t.Fatal("Disarm left a pending refresh")
	}
	// Give a cancelled timer every chance to have fired anyway.
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("a disarmed refresh fired %d times", calls)
	}
}

func TestWarmerArmReplacesPending(t *testing.T) {
	fired := make(chan string, 4)
	w := New(func(ctx context.Context, signature string) error {
		fired <- signature
		return nil
	}, nil)
	w.Arm("first", 30*time.Millisecond)
	w.Arm("second", 30*time.Millisecond)
	select {
	case sig := <-fired:
		if sig != "second" {
			t.Fatalf("fired %q, want the replacement", sig)
		}
	case <-time.After(time.Second):
		t.Fatal("no refresh fired")
	}
	select {
	case sig := <-fired:
		t.Fatalf("the replaced refresh fired as %q", sig)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestWarmerNilSafe(t *testing.T) {
	var w *Warmer
	w.Arm("sig", time.Millisecond)
	w.Disarm()
	if w.Armed() {
		t.Fatal("a nil warmer reported armed")
	}
}

func TestWarmerRefreshErrorIsLoggedNotPanicked(t *testing.T) {
	done := make(chan struct{})
	w := New(func(ctx context.Context, signature string) error {
		defer close(done)
		return errors.New("boom")
	}, func(format string, args ...any) {})
	w.Arm("sig", time.Millisecond)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the refresh never ran")
	}
}
