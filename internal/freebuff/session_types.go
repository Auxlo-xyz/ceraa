package freebuff

import "time"

// Wire header names for the free-mode session endpoints. Shared so the exact
// strings live in one place instead of being retyped at each call site.
const (
	headerInstance             = "x-freebuff-instance-id"
	headerModel                = "x-freebuff-model"
	headerWalletSpendLimit     = "x-freebuff-wallet-spend-limit"
	headerTakeoverInstance     = "x-freebuff-takeover-instance-id"
	headerCompactSession       = "x-freebuff-compact-session"
	headerMultiSession         = "x-freebuff-multi-session"
	headerHeartbeat            = "x-freebuff-heartbeat"
	headerPurchaseContinuity   = "x-freebuff-purchase-continuity"
	headerDesktopAttempt       = "x-freebuff-desktop-attempt-id"
	headerIncludeUnusedRateLim = "x-freebuff-include-unused-rate-limits"
	headerFirstTabDiscount     = "x-freebuff-first-tab-discount"
	headerTimezone             = "x-fb-timezone"
)

const (
	sessionAdmissionPath = "/api/v1/freebuff/session/admission"
	sessionPath          = "/api/v1/freebuff/session"
	sessionAttemptPath   = "/api/v1/freebuff/session/attempt"
)

// sessionUnsupportedMessage is returned verbatim when admission 404/405s. The
// dedicated route fails closed on servers predating the guarantees, so we must
// fail closed too rather than fall back to the legacy path.
const sessionUnsupportedMessage = "This server cannot safely start or resume your session yet. Reload or update Freebuff and try again shortly. No purchase was made."

const (
	// PollIntervalActive is the CLI's active-session poll cadence.
	PollIntervalActive = 30 * time.Second
	// PollJitter is the fraction of the cadence added as random jitter.
	PollJitter = 0.2
	// HeartbeatInterval is the server's expected beat cadence. The server's
	// liveness TTL is a multiple of this, so a brief blip never costs a live
	// session its slot.
	HeartbeatInterval = 45 * time.Second
)

// GateCode is one entry of the server's chat/completions session gate. A
// rejection is identified by the PAIR (code, status) — never by the code alone.
type GateCode struct {
	Status         int
	EndsTheSession bool
}

// gateCodes mirrors FREEBUFF_GATE_CODES exactly.
var gateCodes = map[string]GateCode{
	"waiting_room_required":  {Status: 428, EndsTheSession: true},
	"session_expired":        {Status: 410, EndsTheSession: true},
	"session_superseded":     {Status: 409, EndsTheSession: true},
	"session_model_mismatch": {Status: 409, EndsTheSession: true},
	// The ACCOUNT is over its concurrent budget; this row is fine.
	"session_limit_reached": {Status: 409, EndsTheSession: false},
	// Transient admission race.
	"waiting_room_queued": {Status: 429, EndsTheSession: false},
	// Withdrawn from free mode. Terminal for the request, not session-ending.
	"model_unavailable": {Status: 410, EndsTheSession: false},
}

// LookupGate resolves a gate code. Both halves must match: the code alone is
// attacker-controlled text from an upstream error body, so an inherited name
// like "toString" must not satisfy the check.
func LookupGate(code string, status int) (GateCode, bool) {
	gate, ok := gateCodes[code]
	if !ok {
		return GateCode{}, false
	}
	if gate.Status != status {
		return GateCode{}, false
	}
	return gate, true
}

// SessionResponse is the subset of the server's session union that Ceraa
// branches on. Everything else is ignored rather than ported.
type SessionResponse struct {
	Status      string `json:"status"`
	InstanceID  string `json:"instanceId"`
	Model       string `json:"model"`
	AccessTier  string `json:"accessTier"`
	AdmittedAt  string `json:"admittedAt"`
	ExpiresAt   string `json:"expiresAt"`
	RemainingMs int64  `json:"remainingMs"`
	Error       string `json:"error"`
	Message     string `json:"message"`

	RateLimit           *RateLimitInfo           `json:"rateLimit"`
	RateLimitsByModel   map[string]RateLimitInfo `json:"rateLimitsByModel"`
	Freebucks           *FreebucksInfo           `json:"freebucks"`
	CountryCode         string                   `json:"countryCode"`
	CountryBlockReason  string                   `json:"countryBlockReason"`
	IPPrivacySignals    []string                 `json:"ipPrivacySignals"`
	RetryAfterMs        int64                    `json:"retryAfterMs"`
	CurrentInstanceID   string                   `json:"currentInstanceId"`
	HoldReason          string                   `json:"holdReason"`
	WaitUntilMs         int64                    `json:"waitUntilMs"`
	QueuePosition       int64                    `json:"queuePosition"`
	AvailableModels     []string                 `json:"availableModels"`
	SpendLimitCents     int64                    `json:"spendLimitCents"`
	TotalSpentCents     int64                    `json:"totalSpentCents"`
	SessionLimitReached bool                     `json:"sessionLimitReached"`
	Premium             *PremiumInfo             `json:"premium"`
}

// RateLimitInfo is the per-model session-hours entitlement.
//
// The shape is taken from a live response, not from the TypeScript types: a
// limited-tier account gets a `limit` in hours and a `recentCount` of hours
// already consumed, so remaining is the difference. `pool` is "limited" for a
// region-limited account and the daily pool resets in the server's stated
// timezone, which is not the caller's.
type RateLimitInfo struct {
	Model                string               `json:"model"`
	EntitlementBreakdown EntitlementBreakdown `json:"entitlementBreakdown"`
	Limit                float64              `json:"limit"`
	Pool                 string               `json:"pool"`
	PoolLabel            string               `json:"poolLabel"`
	Period               string               `json:"period"`
	ResetTimeZone        string               `json:"resetTimeZone"`
	ResetAt              string               `json:"resetAt"`
	WindowHours          float64              `json:"windowHours"`
	RecentCount          float64              `json:"recentCount"`
}

// EntitlementBreakdown splits an entitlement into its sources. Streak and
// referral hours are earned over time, so a base-only account is a new one.
type EntitlementBreakdown struct {
	Base     float64 `json:"base"`
	Referral float64 `json:"referral"`
	Streak   float64 `json:"streak"`
}

// RemainingHours is the unconsumed part of the entitlement.
func (r RateLimitInfo) RemainingHours() float64 {
	remaining := r.Limit - r.RecentCount
	if remaining < 0 {
		return 0
	}
	return remaining
}

// FreebucksInfo is the daily-wallet meter. A metered account's whole allowance
// lives here, which is why a compact poll must carry it forward.
type FreebucksInfo struct {
	Balance          float64            `json:"balance"`
	Daily            *DailyPool         `json:"daily"`
	Wallet           *WalletBalance     `json:"wallet"`
	PlanID           *string            `json:"planId"`
	Prices           map[string]float64 `json:"prices"`
	PriceNotices     map[string]string  `json:"priceNotices"`
	OffPeak          map[string]OffPeak `json:"offPeak"`
	PriceChanges     []PriceChange      `json:"priceChanges"`
	FirstTabDiscount *FirstTabDiscount  `json:"firstTabDiscount"`
}

// DailyPool is the daily Freebucks allowance. ResetTimeZone echoes the timezone
// the client advertised in x-fb-timezone, so the meter can reset at the user's
// local midnight rather than at a fixed UTC boundary.
type DailyPool struct {
	Limit         float64 `json:"limit"`
	Spent         float64 `json:"spent"`
	Remaining     float64 `json:"remaining"`
	ResetAt       string  `json:"resetAt"`
	ResetTimeZone string  `json:"resetTimeZone"`
}

type WalletBalance struct {
	Balance      float64 `json:"balance"`
	MonthlyBonus float64 `json:"monthlyBonus"`
}

// OffPeak is a cheaper window for one model, expressed in UTC hours.
type OffPeak struct {
	StartHourUTC int     `json:"startHourUtc"`
	EndHourUTC   int     `json:"endHourUtc"`
	Price        float64 `json:"price"`
	RegularPrice float64 `json:"regularPrice"`
}

// PriceChange is a scheduled price move, so a client can warn before it bites.
type PriceChange struct {
	At      string  `json:"at"`
	ModelID string  `json:"modelId"`
	Price   float64 `json:"price"`
	Tagline string  `json:"tagline"`
}

type FirstTabDiscount struct {
	Available bool `json:"available"`
	Applied   bool `json:"applied"`
}

type PremiumInfo struct {
	Enabled bool   `json:"enabled"`
	Plan    string `json:"plan"`
}

// TerminalStatus reports whether polling should stop for this status.
func (s SessionResponse) TerminalStatus() bool {
	switch s.Status {
	case "none", "superseded", "country_blocked", "banned", "model_locked",
		"model_unavailable", "rate_limited", "spend_limited", "ip_capped",
		"consent_required", "premium_slot_taken", "purchase_claim_released",
		"purchase_in_use", "purchase_capacity", "first_tab_discount_changed":
		return true
	}
	return false
}

// HoldsLiveSlot reports whether this session is currently occupying the
// account's single concurrency slot.
func (s SessionResponse) HoldsLiveSlot() bool {
	return s.Status == "active" || (s.Status == "ended" && s.InstanceID != "")
}

// asSessionError reports whether err is (or wraps) a *SessionError.
func asSessionError(err error, target **SessionError) bool {
	for err != nil {
		if candidate, ok := err.(*SessionError); ok {
			*target = candidate
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
