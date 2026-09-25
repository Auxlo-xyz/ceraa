package freebuff

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"sync"
	"time"
)

// SessionManager owns Sooraa's single hosted Freebuff session: the thing that
// actually grants quota.
//
// Sooraa is one long-lived process for one Telegram account, so it uses the CLI's
// single-session profile rather than Desktop's multi-session protocol: no
// cli:-prefixed instance ids, no purchase continuity, no attempt ids, and no
// takeover. It still sends a plain instance id on GET/DELETE so the server can
// recognize the row, and never on POST.
//
// Startup probes with GET before committing with POST: if a session is already
// live, adopt it rather than rotating the slot for nothing.
type SessionManager struct {
	mu       sync.Mutex
	client   *Client
	instance string
	session  SessionResponse
	// token and model are held in memory only (never persisted, never logged)
	// so a gate recovery can re-admit synchronously without the caller having to
	// thread the credential back in.
	token string
	model string
	// generation invalidates in-flight ticks after a model change.
	generation int
	cancel     context.CancelFunc
	running    bool
	// onChange is invoked on every state transition so the bot can surface quota.
	onChange func(SessionResponse)
	// notify is called when a session-ending gate code forces re-admission.
	notify func(string)
}

func NewSessionManager(client *Client) *SessionManager {
	if client == nil {
		client = NewClient()
	}
	return &SessionManager{client: client}
}

// SetOnChange registers a state-transition callback.
func (m *SessionManager) SetOnChange(fn func(SessionResponse)) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

// SetNotify registers a user-facing message callback (e.g. quota exhausted).
func (m *SessionManager) SetNotify(fn func(string)) {
	m.mu.Lock()
	m.notify = fn
	m.mu.Unlock()
}

// newInstanceID mints an opaque instance id. It is NOT cli:-prefixed: that
// prefix opts into Desktop's multi-session protocol, which a bot has no use for.
func newInstanceID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// Fall back to a time-based id; uniqueness within one host is enough.
		return fmt.Sprintf("sooraa-%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// InstanceID returns the current instance id, creating one on first use.
func (m *SessionManager) InstanceID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.instance == "" {
		m.instance = newInstanceID()
	}
	return m.instance
}

// Current returns the last known session state.
func (m *SessionManager) Current() SessionResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.session
}

// QuotaRemainingHours returns the remaining free hours for the active model, and
// whether the model is unmetered.
func (m *SessionManager) QuotaRemainingHours() (float64, bool, bool) {
	session := m.Current()
	for _, entry := range session.RateLimitsByModel {
		if entry.Model == session.Model {
			return entry.RemainingHours, true, entry.Unlimited
		}
	}
	if session.RateLimit != nil && session.RateLimit.Model == session.Model {
		return session.RateLimit.RemainingHours, true, session.RateLimit.Unlimited
	}
	return 0, false, false
}

// Ensure admits a session for the given model, reusing a live one when the model
// matches. It is safe to call before every run.
func (m *SessionManager) Ensure(ctx context.Context, token, model string) (SessionResponse, error) {
	m.mu.Lock()
	m.token = token
	m.model = model
	m.mu.Unlock()
	current := m.Current()
	if current.Status == "active" && current.Model == model && current.InstanceID != "" {
		return current, nil
	}
	// Probe before committing: a live session on the same model is adopted.
	probe, probeErr := m.probe(ctx, token)
	if probeErr == nil && probe.Status == "active" && probe.Model == model {
		m.adopt(probe)
		return probe, nil
	}
	return m.admit(ctx, token, model)
}

// ReAdmit claims a fresh session using the credentials and model captured by the
// last Ensure. This is the recovery for a session-ending gate: the row is gone,
// so the request must be re-issued against a newly admitted session on the SAME
// instance id. It is a no-op without a prior Ensure, because there is no
// credential to re-admit with.
func (m *SessionManager) ReAdmit(ctx context.Context) error {
	m.mu.Lock()
	token, model := m.token, m.model
	m.mu.Unlock()
	if token == "" || model == "" {
		return errors.New("freebuff has no session credential to re-admit with")
	}
	_, err := m.admit(ctx, token, model)
	return err
}

// probe is a read-only GET that doubles as a heartbeat on an existing row.
func (m *SessionManager) probe(ctx context.Context, token string) (SessionResponse, error) {
	instance := m.InstanceID()
	return m.client.callSession(ctx, sessionRequest{
		method:     "GET",
		token:      token,
		instanceID: instance,
		compact:    false,
	})
}

// admit claims the session slot for a model.
func (m *SessionManager) admit(ctx context.Context, token, model string) (SessionResponse, error) {
	instance := m.InstanceID()
	session, err := m.client.callSession(ctx, sessionRequest{
		method:           "POST",
		token:            token,
		instanceID:       instance,
		model:            model,
		walletSpendLimit: "0",
		// We are not the first tab; claiming the discount would be wrong.
		firstTabDiscount: false,
	})
	if err != nil {
		return SessionResponse{}, err
	}
	m.adopt(session)
	return session, nil
}

// Release ends the session so the slot is freed immediately rather than at
// expiry. Best-effort.
func (m *SessionManager) Release(ctx context.Context, token string) {
	current := m.Current()
	if !current.HoldsLiveSlot() {
		return
	}
	_, _ = m.client.callSession(ctx, sessionRequest{
		method:     "DELETE",
		token:      token,
		instanceID: m.InstanceID(),
	})
	m.mu.Lock()
	m.session = SessionResponse{Status: "none"}
	m.mu.Unlock()
}

// Start begins the background keep-alive loop. It probes, admits when needed,
// then polls so the row's liveness is refreshed and quota state stays fresh.
func (m *SessionManager) Start(ctx context.Context, token, model string) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = true
	m.generation++
	generation := m.generation
	loopCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mu.Unlock()

	if _, err := m.Ensure(ctx, token, model); err != nil {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
		cancel()
		return err
	}
	go m.loop(loopCtx, token, model, generation)
	return nil
}

// Stop halts the keep-alive loop and releases the slot.
func (m *SessionManager) Stop(ctx context.Context, token string) {
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.running = false
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.Release(ctx, token)
}

// Invalidate drops the cached session state without contacting the server. It is
// used when a hosted gate reports the row is gone: the next Ensure must re-admit
// rather than trust the stale row, and the reference's only supported recovery
// is to re-admit on the SAME instance id.
func (m *SessionManager) Invalidate() {
	m.mu.Lock()
	m.session = SessionResponse{Status: "invalidated"}
	m.mu.Unlock()
}

func (m *SessionManager) adopt(session SessionResponse) {
	m.mu.Lock()
	m.session = session
	onChange := m.onChange
	m.mu.Unlock()
	if onChange != nil {
		onChange(session)
	}
	if terminalMessage := sessionTerminalMessage(session); terminalMessage != "" {
		m.mu.Lock()
		notify := m.notify
		m.mu.Unlock()
		if notify != nil {
			notify(terminalMessage)
		}
	}
}

// sessionTerminalMessage turns a refusal into a user-facing explanation. The
// first-tab and purchase states are intentionally omitted: a bot has no tab and
// no purchase, so they cannot occur.
func sessionTerminalMessage(session SessionResponse) string {
	switch session.Status {
	case "country_blocked":
		return "Freebuff is not available from this location for the selected model."
	case "banned":
		return "This Freebuff account is suspended."
	case "rate_limited", "spend_limited":
		return "Freebuff daily allowance is used up. It refills at midnight Pacific."
	case "ip_capped":
		return "Freebuff has capped this IP address's usage. Try again later or from another network."
	case "model_locked":
		return "That model is locked for this account. Pick another model."
	case "model_unavailable":
		return "That model was withdrawn from Freebuff free mode. Pick another model."
	case "premium_slot_taken":
		return "The Freebuff premium slot for that model is taken."
	case "consent_required":
		return "Freebuff needs wallet consent before this model can run."
	}
	return ""
}

func (m *SessionManager) loop(ctx context.Context, token, model string, generation int) {
	for {
		delay := m.nextDelay(m.Current())
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		if m.generation != generation {
			m.mu.Unlock()
			return
		}
		active := m.session
		compact := active.Status == "active" && active.Model == model
		m.mu.Unlock()

		next, err := m.client.callSession(ctx, sessionRequest{
			method:     "GET",
			token:      token,
			instanceID: m.InstanceID(),
			compact:    compact,
		})
		if err != nil {
			// GET is read-only, so transient failures are always safe to retry.
			continue
		}
		// A compact poll omits quota fields; carry them forward for the same
		// live session so the balance does not blank mid-run.
		if compact {
			next = mergeCompactSession(active, next)
		}
		m.adopt(next)
		if next.TerminalStatus() {
			return
		}
		if next.Status == "none" || next.Status == "ended" {
			// The slot lapsed. Re-admit on the SAME instance id.
			if _, admitErr := m.admit(ctx, token, model); admitErr != nil {
				var unsupported *UnsupportedError
				if ok := asUnsupported(admitErr, &unsupported); ok {
					return
				}
			}
		}
	}
}

// mergeCompactSession keeps the quota snapshot a compact poll omits. A mismatch
// of instance or model returns the response unchanged so the caller refetches a
// full one.
func mergeCompactSession(current, next SessionResponse) SessionResponse {
	if current.Status != "active" || next.Status != "active" ||
		current.InstanceID != next.InstanceID || current.Model != next.Model {
		return next
	}
	if next.RateLimit == nil {
		next.RateLimit = current.RateLimit
	}
	if next.RateLimitsByModel == nil {
		next.RateLimitsByModel = current.RateLimitsByModel
	}
	// The Freebucks block is the whole meter for a metered account, so losing
	// it mid-session would drop the quota display to nothing.
	if next.Freebucks == nil {
		next.Freebucks = current.Freebucks
	}
	return next
}

func asUnsupported(err error, target **UnsupportedError) bool {
	if unsupported, ok := err.(*UnsupportedError); ok {
		*target = unsupported
		return true
	}
	return false
}

// nextDelay is the CLI's cadence: 30s plus jitter, clamped so a poll lands just
// after expires_at rather than leaving a stuck countdown.
func (m *SessionManager) nextDelay(session SessionResponse) time.Duration {
	base := float64(PollIntervalActive) * (1 + PollJitter*jitterUnit())
	delay := time.Duration(base)
	switch session.Status {
	case "active":
		if session.RemainingMs > 0 {
			// Land just after expiry so the transition shows up promptly.
			if until := time.Duration(session.RemainingMs)*time.Millisecond + time.Second; until < delay {
				delay = until
			}
		}
		if delay < time.Second {
			delay = time.Second
		}
	case "ended":
		if session.InstanceID == "" {
			return 0
		}
	default:
		return delay
	}
	return delay
}

func jitterUnit() float64 {
	// Bounded pseudo-jitter without pulling math/rand's global state into the
	// poll loop: the phase advances with wall time and stays in [0,1).
	phase := float64(time.Now().UnixNano()%1000) / 1000
	return phase
}
