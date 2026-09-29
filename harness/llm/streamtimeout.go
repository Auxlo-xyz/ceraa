package llm

import (
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// A streaming request has two quite different deadlines and Go only offers one of
// them on http.Client. Timeout is total: it covers the connection, the headers,
// and every byte of the body, and it is set once when the request starts. On an
// SSE stream that is a budget for the whole answer rather than for the wait
// between chunks, so a model that is thinking for two minutes before its first
// token is indistinguishable from one that has stopped responding.
//
// That is not hypothetical. A Gemini model at HIGH reasoning with a long prompt
// routinely runs past two minutes, and the client killed it mid-answer. The retry
// loop then did the rest of the damage: the timeout arrives as a *url.Error
// satisfying net.Error with no status, so IsGatewayError cannot tell it from a
// provider outage and classified it as one, buying it 5s/15s/45s of gateway
// backoff. Eight minutes of wall clock to deliver the same failure twice more.
//
// So the total budget is gone and the same number is applied where it means
// something: as the longest the provider may stay silent. Every byte resets it.

// idleTimeoutTransport bounds the wait for response headers, which is the part of
// the exchange that happens before any streaming begins and cannot be reset by
// body traffic.
func idleTimeoutTransport(idle time.Duration) http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if idle > 0 {
		transport.ResponseHeaderTimeout = idle
	}
	return transport
}

// idleTimeoutBody fails a read that makes no progress for the idle window.
//
// Resetting a deadline around a blocking Read is not enough on its own: nothing
// observes the timer while the goroutine is parked inside the transport, so a
// wedged connection would sit there forever. The window is therefore armed with a
// watchdog that closes the underlying body, which is what actually unblocks the
// pending read, and the close is reported as the stall it was rather than
// surfacing as "read on closed response body".
type idleTimeoutBody struct {
	body io.ReadCloser
	idle time.Duration
	// stalled is set by the watchdog before it closes the body, so Read can tell
	// its own interrupt apart from a transport error that happened to arrive.
	stalled atomic.Bool
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	timer := time.NewTimer(b.idle)
	done := make(chan struct{})
	go func() {
		select {
		case <-timer.C:
			b.stalled.Store(true)
			b.body.Close()
		case <-done:
		}
	}()

	n, err := b.body.Read(p)
	close(done)
	timer.Stop()

	if b.stalled.Load() {
		return n, fmt.Errorf("provider sent nothing for %s; the stream stalled rather than the request timing out", b.idle)
	}
	return n, err
}

func (b *idleTimeoutBody) Close() error { return b.body.Close() }

// doRequest performs the exchange and arms the stall watchdog on the body.
//
// Every transport goes through here rather than calling c.http.Do directly, so
// the protection cannot be forgotten on the next transport added. A caller that
// supplies its own HTTPClient still gets it: the window lives on the body, not on
// the client.
func (c *StreamingClient) doRequest(request *http.Request) (*http.Response, error) {
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	if c.cfg.Timeout > 0 && response.Body != nil {
		response.Body = &idleTimeoutBody{body: response.Body, idle: c.cfg.Timeout}
	}
	return response, nil
}
