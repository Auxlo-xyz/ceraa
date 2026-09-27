package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A 502 means the provider's upstream is unreachable, so the wait has to be long
// enough to matter. The default schedule spans about twelve seconds in total,
// which is what a live run burned through before dying to an intermittent gateway.
func TestGatewayErrorsBackOffHarderThanRateLimits(t *testing.T) {
	gateway := retryDelay(&ProviderError{Status: 502}, 0, 0)
	rateLimit := retryDelay(&ProviderError{Status: 429}, 0, 0)
	if gateway <= rateLimit {
		t.Errorf("gateway backoff %v is not longer than the rate-limit backoff %v", gateway, rateLimit)
	}
	if total := retryDelay(&ProviderError{Status: 502}, 0, 0) +
		retryDelay(&ProviderError{Status: 502}, 1, 0) +
		retryDelay(&ProviderError{Status: 502}, 2, 0); total < 30*time.Second {
		t.Errorf("three gateway retries span only %v; that does not cover a real outage", total)
	}
}

// The backoff must strictly grow, or a sick gateway is hammered at a constant rate.
// Strictly, not merely non-decreasing: a flat schedule is the failure mode here, and
// a <= comparison would accept it.
func TestGatewayBackoffStrictlyIncreases(t *testing.T) {
	prev := time.Duration(0)
	for attempt := 0; attempt < len(gatewayBackoff); attempt++ {
		d := retryDelay(&ProviderError{Status: 503}, attempt, 0)
		if d <= prev {
			t.Errorf("attempt %d waits %v, not longer than the previous %v", attempt, d, prev)
		}
		prev = d
	}
}

// Beyond the schedule it must plateau rather than run off into a huge or zero wait.
func TestGatewayBackoffPlateaus(t *testing.T) {
	last := retryDelay(&ProviderError{Status: 502}, len(gatewayBackoff)-1, 0)
	beyond := retryDelay(&ProviderError{Status: 502}, len(gatewayBackoff)+5, 0)
	if beyond != last {
		t.Errorf("past the schedule the wait changed: %v then %v", last, beyond)
	}
}

// A 4xx is the caller's fault. Backing off as if it were an outage wastes the
// budget on a request that cannot succeed.
func TestClientErrorsAreNotTreatedAsGatewayFaults(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422} {
		if IsGatewayError(&ProviderError{Status: status}) {
			t.Errorf("HTTP %d was classified as a gateway fault", status)
		}
	}
	for _, status := range []int{500, 502, 503, 504} {
		if !IsGatewayError(&ProviderError{Status: status}) {
			t.Errorf("HTTP %d was not classified as a gateway fault", status)
		}
	}
}

// A cancelled context is the user stopping the run, not a fault to retry through.
func TestCancellationIsNotAGatewayFault(t *testing.T) {
	if IsGatewayError(context.Canceled) {
		t.Error("cancellation was treated as a gateway fault")
	}
	if IsGatewayError(context.DeadlineExceeded) {
		t.Error("a deadline was treated as a gateway fault")
	}
	if IsGatewayError(nil) {
		t.Error("nil was treated as a gateway fault")
	}
}

// A transport fault never reached the provider, so it is an outage.
func TestTransportFaultsCountAsGatewayFaults(t *testing.T) {
	var netErr net.Error = &net.DNSError{Err: "no such host", Name: "x"}
	if !IsGatewayError(fmt.Errorf("post: %w", netErr)) {
		t.Error("a DNS failure was not treated as a gateway fault")
	}
	if IsGatewayError(errors.New("plain failure")) {
		t.Error("an unclassified error was treated as a gateway fault")
	}
}

// Tests must not actually sleep for a minute.
func TestRetryBackoffScaleOverridesTheSchedule(t *testing.T) {
	if got := retryDelay(&ProviderError{Status: 502}, 2, time.Millisecond); got != time.Millisecond {
		t.Errorf("the test scale was ignored: %v", got)
	}
}

// The wait has to be reported, or a minute of silence looks like a hang.
func TestRetriesAreReportedWithTheirWait(t *testing.T) {
	type call struct {
		attempt int
		delay   time.Duration
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream failure"}}`))
	}))
	defer srv.Close()

	c := New(ProviderConfig{
		BaseURL: srv.URL, MaxRetries: 2, APIKey: "k", RetryBackoff: time.Millisecond,
		HTTPClient: srv.Client(),
		OnRetry: func(_ error, attempt int, delay time.Duration) {
			calls = append(calls, call{attempt: attempt, delay: delay})
		},
	})
	_ = c.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(ProviderResponse) {})

	if len(calls) == 0 {
		t.Fatal("no retry was reported, so a long wait would be invisible")
	}
	for i, got := range calls {
		if got.attempt != i+1 {
			t.Errorf("retry %d reported attempt %d", i, got.attempt)
		}
		if got.delay <= 0 {
			t.Errorf("retry %d reported a %v wait", i, got.delay)
		}
	}
}
