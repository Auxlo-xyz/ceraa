package recon

// scope.go is the authorization layer the probe and scanner sit under.
//
// The division of labor is deliberate: Gate answers "is this destination
// authorized", netguard answers "is this destination on the host's own
// network". A name can answer differently between the two checks, which is
// why the dial-time netguard check exists; a scope record that says
// "example.com is the engagement" is what keeps the dial from happening at
// all. Neither is trusted alone.
//
// One engagement is active per process. This is a single-operator bot, and a
// model juggling two missions at once reads like subtlety but behaves like
// drift, so the record is replaced, never merged.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ceraa/harness/netguard"
	"ceraa/internal/logger"
	"ceraa/internal/paths"
)

var (
	ErrBadURL      = errors.New("bad url")
	ErrNoMission   = errors.New("no active engagement")
	ErrSelfTarget  = errors.New("self-target, the host's own machine")
	ErrOutOfScope  = errors.New("out of scope")
	ErrRateLimited = errors.New("rate limited")
	ErrExhausted   = errors.New("budget exhausted")
)

// Engagement is one authorized mission. The fields the model cites back
// (target, objective) are display; the fields the gate consumes are the
// allowlist, the denylist, and the two budgets.
type Engagement struct {
	// Target is the engagement's base URL. With an empty InScope it defines
	// the authorization: the target host and every subdomain of it.
	Target string
	// Objective is recorded for the receipts the operator reads later, never
	// enforced by the gate.
	Objective string
	// InScope holds allowlist tokens. Empty means the target itself and its
	// subdomains. A token is a host ("example.com"), a leading-dot suffix
	// that also covers subdomains (".example.com"), or a host with a
	// required port ("example.com:8443"). A loopback or private-address
	// token typed literally (for example "127.0.0.1:9443") is the operator's
	// own, explicit authorization of an internal target: the self-target
	// refusal below applies only to hosts that arrive without such a token.
	InScope []string
	// OutScope holds denylist tokens in the same shapes. It always wins.
	OutScope []string
	// UserAgent, when set, is the engagement's required identifier, to be
	// attached by every request the harness sends. The sender enforcement in
	// the probe is what makes it real; this field is the record.
	UserAgent string
	// RPM caps requests per rolling 60s window. 0 means uncapped. A rolling
	// window keeps short bursts sane without ever letting sustained volume
	// through.
	RPM int
	// RequestCap is the total request ceiling for the engagement. 0 means
	// uncapped. Take increments it and nothing rolls it back, because the
	// requests already went out.
	RequestCap int
	// AllowDestructive records the operator's authorization level. The probe
	// is inherently observe-only, so nothing enforces this yet, but the
	// record travels with the receipts.
	AllowDestructive bool
	// AllowPrivate names the private ranges the operator authorized for this
	// engagement: hosts, addresses or CIDRs that the egress wall would
	// otherwise refuse, because an internal engagement lives there.
	//
	// It is the operator's decision and it travels with the record, so the
	// wall opens and closes with the engagement instead of with a restart.
	AllowPrivate []string
	// Active is always true in a stored record: Stop clears the record
	// rather than writing this field false.
	Active bool
	// CreatedAt records when the engagement was set, for the receipt trail.
	CreatedAt int64
}

var (
	scopeMu sync.RWMutex
	active  *Engagement
	// window and takenInWindow are the rate budget. The window resets only
	// when it actually passes.
	window        time.Time
	takenInWindow int
	// takenTotal is the lifetime counter for RequestCap. It survives window
	// resets by design.
	takenTotal int
)

// scopeFile is the on-disk record. Writes are atomic so a crash mid-write
// can never leave a partial record a restart misreads as authorization.
func scopeFile() string {
	return filepath.Join(paths.DataDir(), "recon-scope.json")
}

// SetActive installs an engagement and resets both budgets. The Target may
// be empty when the engagement is defined purely by explicit InScope tokens
// (for example an internal lab box named as a literal loopback host); a bare
// self-target token then binds to whatever port the request names. Writing
// the record is best-effort: an unwritable disk degrades the forensic trail,
// not the authorization itself, which lives in memory from this call on.
func SetActive(e Engagement) error {
	target := strings.TrimSpace(e.Target)
	if target != "" {
		parsed, err := url.Parse(target)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("%w: engagement target must be an absolute http(s) url", ErrBadURL)
		}
	}
	now := time.Now().Unix()
	scopeMu.Lock()
	defer scopeMu.Unlock()
	active = &e
	active.Active = true
	if active.CreatedAt == 0 || active.CreatedAt > now {
		active.CreatedAt = now
	}
	window = time.Time{}
	takenInWindow = 0
	takenTotal = 0
	applyPrivateRanges(*active)
	if err := persistLocked(); err != nil {
		logger.Warn("recon: scope record not written: " + err.Error())
	}
	return nil
}

// Active returns the active engagement. The on-disk record is consulted only
// when memory holds none, so a call right after a process restart reads the
// file the previous run wrote.
func Active() (Engagement, bool) {
	scopeMu.RLock()
	defer scopeMu.RUnlock()
	if active != nil {
		copied := *active
		applyPrivateRanges(copied)
		return copied, true
	}
	e, ok := readScopeFile()
	if ok {
		applyPrivateRanges(e)
	}
	return e, ok
}

// applyPrivateRanges hands the operator's authorized ranges to the egress
// wall.
//
// It is called on every read of the record rather than once at write time,
// because the record can be written or cleared by another process while the
// daemon runs, and the wall has to reflect the record as it stands now. An
// engagement with no authorized ranges clears the list, so ending an
// engagement ends the reach it granted.
func applyPrivateRanges(e Engagement) {
	netguard.SetAuthorizedRanges(e.AllowPrivate)
}

// Stop clears the active engagement and deletes the on-disk record, so a
// restarted process does not resurrect an authorization the operator ended.
// The file is the receipt trail's source of truth for "was anything live",
// and a record that outlives its owner is a record that lies.
func Stop() {
	scopeMu.Lock()
	defer scopeMu.Unlock()
	active = nil
	window = time.Time{}
	takenInWindow = 0
	takenTotal = 0
	// Ending the engagement ends the reach it granted.
	netguard.SetAuthorizedRanges(nil)
	if err := os.Remove(scopeFile()); err != nil && !os.IsNotExist(err) {
		logger.Warn("recon: scope record not cleared: " + err.Error())
	}
}

// persistLocked writes the active record atomically. The caller holds the
// lock, because the write races the budgets, not the disk.
func persistLocked() error {
	data, err := json.Marshal(active)
	if err != nil {
		return err
	}
	dir := filepath.Dir(scopeFile())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".recon-scope.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, scopeFile())
}

// readScopeFile loads the on-disk record for the post-restart case. A record
// that is not Active, or that carries no target, is no record at all.
func readScopeFile() (Engagement, bool) {
	data, err := os.ReadFile(scopeFile())
	if err != nil || len(data) < 3 {
		return Engagement{}, false
	}
	var e Engagement
	if err := json.Unmarshal(data, &e); err != nil {
		return Engagement{}, false
	}
	// A record is a record when it is active and names something: a target,
	// or explicit scope tokens for an internal box. SetActive accepts a
	// token-only engagement, so the file read has to accept one too.
	if !e.Active || (e.Target == "" && len(e.InScope) == 0) {
		return Engagement{}, false
	}
	return e, true
}

// Gate decides whether one URL may leave the machine.
//
// The check order is the point. A denylist built after an allowlist is a
// suggested denylist; a self-target rule that runs only after a scope match
// is an inference, not a rule. So: mission first, then parse, then the
// explicit token the operator typed, then self, then deny, then allow.
func Gate(rawurl string) error {
	scopeMu.RLock()
	var e *Engagement
	if active != nil {
		copied := *active
		e = &copied
	} else if o, ok := readScopeFile(); ok {
		e = &o
	}
	scopeMu.RUnlock()
	if e != nil {
		// The record can have been written or cleared by another process
		// since the last read, and the wall has to reflect it now.
		applyPrivateRanges(*e)
	}

	parsed, err := url.Parse(strings.TrimSpace(rawurl))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%w: %s", ErrBadURL, rawurl)
	}
	host := canonicalHost(parsed)
	port := urlPort(parsed)

	if e == nil {
		// No engagement: observation-grade access to the public web, the
		// same floor web_fetch already has. The host's own machine stays
		// refused, because nothing about an absent record authorizes it.
		if isSelfTargetHost(host) {
			return fmt.Errorf("%w: %s", ErrSelfTarget, host)
		}
		return nil
	}

	// An explicit literal token is the operator authorizing this exact
	// endpoint, including an internal one. It outranks the self-target rule,
	// because "the lab box is the mission" is a real answer: what the token
	// cannot do is arrive via a wildcard. A bare self-target host token
	// binds to the engagement's own port when the Target names one, so the
	// authorization covers the endpoint the competitor named, never the
	// whole loopback.
	if _, explicit := literalTokenMatch(e.InScope, host, port, engagementPort(e.Target)); explicit {
		return denyCheck(e, parsed)
	}
	if isSelfTargetHost(host) {
		return fmt.Errorf("%w: %s", ErrSelfTarget, host)
	}
	return denyCheck(e, parsed)
}

// engagementPort extracts the port the engagement's Target binds. Empty
// when there is no Target or when it carries no port.
func engagementPort(target string) string {
	t := strings.TrimSpace(target)
	if t == "" {
		return ""
	}
	parsed, err := url.Parse(t)
	if err != nil {
		return ""
	}
	return parsed.Port()
}

// denyCheck applies the denylist and then the allowlist, in that order.
// Separated from Gate so the explicit-token path and the ordinary path share
// one matching story instead of two that drift.
func denyCheck(e *Engagement, parsed *url.URL) error {
	host := canonicalHost(parsed)
	port := urlPort(parsed)
	if matchTokens(e.OutScope, host, port) {
		return fmt.Errorf("%w: %s is on the out-of-scope list", ErrOutOfScope, host)
	}
	targetHost := ""
	if t, err := url.Parse(strings.TrimSpace(e.Target)); err == nil {
		targetHost = canonicalHost(t)
	}
	if len(e.InScope) == 0 && targetHost != "" {
		if host == targetHost || strings.HasSuffix(host, "."+targetHost) {
			return nil
		}
		return fmt.Errorf("%w: %s is not %s or a subdomain of it", ErrOutOfScope, host, targetHost)
	}
	if matchTokens(e.InScope, host, port) {
		return nil
	}
	return fmt.Errorf("%w: %s does not match any in-scope token", ErrOutOfScope, host)
}

// canonicalHost lowercases and strips IPv6 brackets, so the same host text
// written three ways matches the same token.
func canonicalHost(parsed *url.URL) string {
	h := strings.ToLower(parsed.Hostname())
	return strings.Trim(h, "[]")
}

// urlPort returns the effective port, filling the scheme's default in, so
// host:port tokens stay comparable between an https url with no explicit
// port and a token written "example.com:8443".
func urlPort(parsed *url.URL) string {
	if p := parsed.Port(); p != "" {
		return p
	}
	if parsed.Scheme == "https" {
		return "443"
	}
	return "80"
}

// isSelfTargetHost refuses hosts that are always the machine running this
// harness: loopback, unspecified, link-local, CGNAT (100.64.0.0/10, where a
// hosted box's hypervisor management interface lives, and Go's IsPrivate
// does not cover), and RFC 1918 private space.
//
// DNS rebinding is deliberately NOT chased by name here. The dial-time
// netguard check covers it, and a resolver call would put network I/O inside
// the authorization gate, which is exactly where it must not be.
func isSelfTargetHost(host string) bool {
	h := strings.Trim(strings.ToLower(host), "[]")
	h = strings.TrimSuffix(h, ".")
	if h == "localhost" || h == "" {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10: first octet 100, second octet 64..127.
		return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
	}
	return false
}

// literalTokenMatch reports whether a token names this host and port written
// out. Only exacts count: a wildcard token that happens to cover the host is
// NOT the operator's claim about an internal endpoint, and pretending it is
// makes the self-target refusal evadable by asking for a broader pattern to
// be added. A bare self-target host token without a port binds to
// engagementPort when that is set; on an engagement with no Target the
// operator named a host, not a port, and the request's port is honored.
func literalTokenMatch(tokens []string, host, port, engagementPort string) (string, bool) {
	for _, tok := range tokens {
		t := strings.ToLower(strings.TrimSpace(tok))
		t = strings.TrimSuffix(t, ".")
		if t == "" {
			continue
		}
		thost, tport := splitHostPort(t)
		if thost != host || !isSelfTargetHost(host) {
			continue
		}
		switch {
		case tport != "":
			if tport == port {
				return t, true
			}
		case engagementPort != "":
			if port == engagementPort {
				return t, true
			}
		default:
			return t, true
		}
	}
	return "", false
}

// matchTokens is the ordinary allow/denylist match: host equality, leading-
// dot subdomain wildcard, or a host:port pair. Scheme is never part of a
// token. A host found under a wider token is inside the engagement whether
// the token came from InScope or OutScope; the denylist only ever wins.
func matchTokens(tokens []string, host, port string) bool {
	for _, tok := range tokens {
		t := strings.ToLower(strings.TrimSpace(tok))
		t = strings.TrimSuffix(t, ".")
		if t == "" {
			continue
		}
		thost, tport := splitHostPort(t)
		if tport != "" && tport != port {
			continue
		}
		if strings.HasPrefix(thost, ".") {
			// ".example.com" covers example.com itself and every depth of
			// subdomain under it.
			bare := strings.TrimPrefix(thost, ".")
			if host == bare || strings.HasSuffix(host, "."+bare) {
				return true
			}
			continue
		}
		if thost == host || strings.HasSuffix(host, "."+thost) {
			return true
		}
	}
	return false
}

// splitHostPort reads a token that may carry its own port. IPv6 tokens keep
// their brackets meaningful, so bracket-stripping happens only when a port
// is actually present.
func splitHostPort(token string) (string, string) {
	if strings.HasPrefix(token, "[") {
		if end := strings.Index(token, "]"); end > 0 {
			rest := token[end+1:]
			if strings.HasPrefix(rest, ":") {
				return token[1:end], rest[1:]
			}
			return token[1:end], ""
		}
		return token, ""
	}
	if i := strings.LastIndex(token, ":"); i >= 0 && isPortDigits(token[i+1:]) {
		return token[:i], token[i+1:]
	}
	return token, ""
}

func isPortDigits(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Take consumes n send slots against the engagement's two budgets, BEFORE
// the request dials, so a refusal means the request never happens. The rate
// window is rolling 60s and resets lazily; the total cap never resets,
// because the requests already happened.
func Take(n int) error {
	scopeMu.Lock()
	defer scopeMu.Unlock()
	// The on-disk record counts: an engagement set by the operator from
	// another process (the recon CLI) is the same engagement, and a budget
	// that ignores it is theatre. The file is read, never cached into the
	// global, so clearing the record from the CLI really clears it here.
	var eng *Engagement
	if active != nil {
		copied := *active
		eng = &copied
	} else if e, ok := readScopeFile(); ok {
		eng = &e
	}
	if eng == nil {
		// No engagement means no metered mission: a plain probe is
		// observation, and there is no budget to charge it to.
		return nil
	}
	if n <= 0 {
		return nil
	}
	now := time.Now()
	if window.IsZero() || now.Sub(window) >= time.Minute {
		window = now
		takenInWindow = 0
	}
	if eng.RPM > 0 && takenInWindow+n > eng.RPM {
		return fmt.Errorf("%w: %d/%d sent this window", ErrRateLimited, takenInWindow, eng.RPM)
	}
	if eng.RequestCap > 0 && takenTotal+n > eng.RequestCap {
		return fmt.Errorf("%w: %d/%d sent on this engagement", ErrExhausted, takenTotal, eng.RequestCap)
	}
	takenInWindow += n
	takenTotal += n
	return nil
}
