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

// The refresh decision is a ratio of prices, not a dollar figure: the token
// count scales both sides of the comparison and cancels. That is what lets it be
// made for a model the catalogue does not carry, which is every model on a
// gateway that reports no catalogue entry.
func TestDecideIgnoresTheAbsolutePriceScale(t *testing.T) {
	now := time.Now()
	// Far enough apart that neither reading is at the dollar floor, so the only
	// thing that can decide is the ratio.
	cheap := Decide(now, now, DefaultTTL, 50000, Economics{InputPerMTok: 3, CacheReadPerMTok: 0.3})
	big := Decide(now, now, DefaultTTL, 50000, Economics{InputPerMTok: 3000, CacheReadPerMTok: 300})
	if cheap.Warm != big.Warm {
		t.Fatalf("the same discount decided differently at two price scales: %v vs %v (%s, %s)",
			cheap.Warm, big.Warm, cheap.Reason, big.Reason)
	}
	// Scaling the prices down until the dollar floor is the only thing left must
	// flip it, which is the floor doing its own separate job.
	floor := Decide(now, now, DefaultTTL, 50000, Economics{InputPerMTok: 3e-6, CacheReadPerMTok: 0.3e-6})
	if floor.Warm {
		t.Fatalf("a saving far below a cent was accepted: %s", floor.Reason)
	}
}

// A model with no catalogue price still gets the keepalive. This is the case
// that disabled it: every run on space-bunny-free logged "pricing unusable" and
// every idle gap lost the whole prompt cache.
func TestDecideWarmsAnUnpricedModel(t *testing.T) {
	now := time.Now()
	d := Decide(now, now, DefaultTTL, 100000, UnknownEconomics)
	if !d.Warm {
		t.Fatalf("an unpriced model was not kept warm: %s", d.Reason)
	}
}

// And the assumed discount is the conservative one: a provider that barely
// discounts caching must still be refused, because the saving would not pay for
// the refresh.
func TestDecideRefusesWhenTheAssumedDiscountWouldNotPay(t *testing.T) {
	now := time.Now()
	// Below ContinuationProbability*MinSavingsMultiple, i.e. no useful discount.
	d := Decide(now, now, DefaultTTL, 100000, Economics{InputPerMTok: 1.2, CacheReadPerMTok: 1})
	if d.Warm {
		t.Fatal("a refresh with almost nothing to save was scheduled")
	}
	if d.Reason != "expected saving below the floor" {
		t.Fatalf("unexpected reason: %s", d.Reason)
	}
}
