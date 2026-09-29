package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A stream that keeps producing tokens must not be cut off part way through.
//
// The client set http.Client.Timeout, which is a budget for the whole exchange -
// connection, headers and every byte of the body - decided once when the request
// started. A model reasoning at HIGH can go well past two minutes before its
// first token, and nothing in a token stream resets a deadline set before the
// first byte arrived. So a model that was working, and visibly working, was
// reported as having timed out.
//
// The test spends real time on purpose. A synthetic clock would not prove the
// behaviour, since the defect is precisely that time passes while data flows.
func TestALongStreamIsNotCutOffWhileItKeepsSending(t *testing.T) {
	// Well inside what the old total deadline allowed, spread across chunks, so a
	// client carrying the old budget dies part way through this stream.
	const idle = 500 * time.Millisecond
	const chunks = 8
	const gap = 150 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(gap):
			}
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		// The chat-completions reader treats a body that ends without a terminal
		// event as a truncated stream, so a well-formed stream has to send one.
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	// Through New, because that is where the deadline is chosen. Building the
	// client by hand would test the stub rather than the decision.
	client := New(ProviderConfig{
		Model: "gemini-3.5-flash", BaseURL: server.URL,
		Timeout: idle, MaxRetries: 2, RetryBackoff: time.Millisecond,
	})

	var got strings.Builder
	err := client.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(r ProviderResponse) { got.WriteString(r.Content) })

	if err != nil {
		t.Fatalf("a stream that kept sending for %s was cut off: %v", time.Duration(chunks)*gap, err)
	}
	if got.Len() != chunks {
		t.Errorf("received %d chunk(s), want %d: the stream was truncated rather than killed", got.Len(), chunks)
	}
}

// stalledServer emits one event and then goes silent forever.
//
// The silence ends when the client closes the body, which is what lets
// server.Close return; a handler that blocked unconditionally would hang the
// test rather than fail it.
func stalledServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return server
}

// The other half, and the reason this was not fixed by deleting the deadline: a
// provider that stops sending must still fail.
//
// Removing the total budget without adding an idle window trades a wrong failure
// for a hang, which is worse: the run holds the bot until something else cancels
// it.
func TestAStalledStreamStillFails(t *testing.T) {
	const idle = 250 * time.Millisecond
	server := stalledServer(t)

	client := New(ProviderConfig{
		Model: "gemini-3.5-flash", BaseURL: server.URL,
		Timeout: idle, MaxRetries: 0,
	})

	started := time.Now()
	err := client.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(ProviderResponse) {})

	if err == nil {
		t.Fatal("a stream that stopped sending reported success")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("the stall took %s to surface; the idle window is %s", elapsed, idle)
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("the failure does not name the stall: %v", err)
	}
}

// The stall must not be mistaken for a gateway outage.
//
// The timeout used to arrive as a *url.Error carrying no status, so
// IsGatewayError could not distinguish it from a 503 and answered true. The run
// then bought 5s/15s/45s of gateway backoff - eight minutes of wall clock to
// deliver the same failure twice more.
func TestAStalledStreamIsNotClassifiedAsAGatewayOutage(t *testing.T) {
	const idle = 250 * time.Millisecond
	server := stalledServer(t)

	client := New(ProviderConfig{
		Model: "gemini-3.5-flash", BaseURL: server.URL,
		Timeout: idle, MaxRetries: 0,
	})

	err := client.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(ProviderResponse) {})

	if err == nil {
		t.Fatal("a stalled stream reported success")
	}
	if IsGatewayError(err) {
		t.Errorf("a stall was classified as a gateway outage, so it buys 5s/15s/45s of backoff: %v", err)
	}
}

// A caller that supplies its own client still gets the stall window, because the
// watchdog is armed on the response body rather than on the client.
func TestAnOverriddenClientStillGetsTheStallWindow(t *testing.T) {
	const idle = 200 * time.Millisecond
	server := stalledServer(t)

	client := New(ProviderConfig{
		Model: "gemini-3.5-flash", BaseURL: server.URL,
		Timeout: idle, MaxRetries: 0,
		HTTPClient: &http.Client{}, // caller-supplied, with no timeout of its own
	})

	started := time.Now()
	err := client.Stream(context.Background(), StreamRequest{
		Messages: []Item{{Kind: "text", Role: RoleUser, Text: "hi"}},
	}, func(ProviderResponse) {})

	if err == nil {
		t.Fatal("an overridden client lost the stall window entirely")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("the stall took %s to surface behind a caller-supplied client", elapsed)
	}
}

// Zero disables the window rather than failing every request instantly. Some
// deployments sit behind a proxy that legitimately holds a stream open longer
// than any sensible default.
func TestZeroTimeoutLeavesTheBodyUnwrapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client := New(ProviderConfig{Model: "m", BaseURL: server.URL, Timeout: 0, MaxRetries: 0})
	request, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.doRequest(request)
	if err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	defer response.Body.Close()

	if _, wrapped := response.Body.(*idleTimeoutBody); wrapped {
		t.Error("a zero Timeout still armed the stall watchdog")
	}
}
