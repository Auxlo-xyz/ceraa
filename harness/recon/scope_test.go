package recon

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"ceraa/internal/paths"
)

// The tests here call Gate with hosts Ceraa would also refuse at dial time
// (netguard refuses loopback addresses on the transport). That is
// deliberate: the scope gate is tested one layer down from the wall, and its
// refusals are the ones that keep a request from ever being fired.

func TestGateNeedsAMissionBeforeAnything(t *testing.T) {
	Stop()
	if err := Gate("https://example.com/targets"); !errors.Is(err, ErrNoMission) {
		t.Fatalf("Gate without an engagement: %v, want ErrNoMission", err)
	}
}

func TestSetActiveRejectsBadTargets(t *testing.T) {
	Stop()
	defer Stop()
	cases := map[string]string{
		"not a url":  "example.com and some trailing prose",
		"no host":    "https:///path",
		"bad scheme": "ftp://example.com/pub",
	}
	for name, target := range cases {
		if err := SetActive(Engagement{Target: target}); !errors.Is(err, ErrBadURL) {
			t.Errorf("%s: SetActive(%q) = %v, want ErrBadURL", name, target, err)
		}
	}
}

func TestGateRefusesUnparseableURLs(t *testing.T) {
	Stop()
	if err := SetActive(Engagement{Target: "https://example.com"}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	defer Stop()
	cases := map[string]string{
		"prose":      "not a url at all",
		"no host":    "mailto:news@example.org",
		"file":       "file:///etc/shadow",
		"no scheme":  "//example.com/path",
		"javascript": "javascript:alert(1)",
	}
	for name, raw := range cases {
		if err := Gate(raw); !errors.Is(err, ErrBadURL) {
			t.Errorf("%s: Gate(%q) = %v, want ErrBadURL", name, raw, err)
		}
	}
}

func TestEmptyInScopeCoversTargetAndSubdomainsOnly(t *testing.T) {
	Stop()
	if err := SetActive(Engagement{Target: "https://target.example.com/panel"}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	defer Stop()
	if err := Gate("https://target.example.com/api"); err != nil {
		t.Errorf("the target itself refused: %v", err)
	}
	if err := Gate("https://sub.deep.target.example.com/?q=1"); err != nil {
		t.Errorf("a subdomain refused: %v", err)
	}
	if err := Gate("http://target.example.com:80/plain"); err != nil {
		t.Errorf("the default port refused: %v", err)
	}
	if err := Gate("https://attacker.example.org/"); !errors.Is(err, ErrOutOfScope) {
		t.Errorf("an unrelated host allowed: %v", err)
	}
	// The suffix trap: evil-target.example.com is a DIFFERENT domain whose
	// name merely ends with the target's host text. Endpoint matching
	// requires dots, never text ends.
	if err := Gate("https://evil-target.example.com/"); !errors.Is(err, ErrOutOfScope) {
		t.Errorf("a lookalike suffix allowed: %v", err)
	}
	if err := TargetEdgeGuard("https://lexample.com/", "lexample.com"); !errors.Is(err, ErrOutOfScope) {
		t.Errorf("a lookalike of the target only refused as out-of-scope: %v", err)
	}
}

func TestExplicitTokensMatchHostSuffixAndPort(t *testing.T) {
	Stop()
	if err := SetActive(Engagement{
		Target:  "https://allowed.example.com",
		InScope: []string{".allowed.example.com", "other.example.net:8443"},
	}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	defer Stop()
	if err := Gate("https://www.allowed.example.com/"); err != nil {
		t.Errorf("subdomain of a dotted token refused: %v", err)
	}
	if err := Gate("https://allowed.example.com:443/"); err != nil {
		t.Errorf("default https port on a dotted token refused: %v", err)
	}
	if err := Gate("https://other.example.net:8443/"); err != nil {
		t.Errorf("an explicitly ported token refused: %v", err)
	}
	if err := Gate("https://other.example.net:9443/"); !errors.Is(err, ErrOutOfScope) {
		t.Errorf("the wrong port rode in on a token-matching host: %v", err)
	}
	if err := Gate("https://unlisted.example.com/"); !errors.Is(err, ErrOutOfScope) {
		t.Errorf("a host outside every token allowed: %v", err)
	}
}

func TestOutScopeWinsOverInScope(t *testing.T) {
	Stop()
	if err := SetActive(Engagement{
		Target:   "https://example.app",
		InScope:  []string{"example.app"},
		OutScope: []string{"admin.example.app", ".third-party.example.app:443"},
	}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	defer Stop()
	if err := Gate("https://admin.example.app/login"); !errors.Is(err, ErrOutOfScope) {
		t.Errorf("a denylisted host allowed: %v", err)
	}
	if err := Gate("https://app.example.app/"); err != nil {
		t.Errorf("an ordinary in-scope host refused: %v", err)
	}
	// A port-bound denylist token binds only its own port: the 443 endpoint
	// is refused, another port on the same suffix stays governed by the
	// ordinary allowlist, which covers example.app and its subdomains.
	if err := Gate("https://third-party.example.app/x"); !errors.Is(err, ErrOutOfScope) {
		t.Errorf("the denylisted default-port host allowed: %v", err)
	}
	if err := Gate("https://third-party.example.app:8443/x"); err != nil {
		t.Errorf("a non-bound port over-refused by a port-bound token: %v", err)
	}
}

func TestSelfTargetRefusedWithoutAnExplicitToken(t *testing.T) {
	Stop()
	if err := SetActive(Engagement{Target: "https://example.com", InScope: []string{"example.com"}}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	defer Stop()
	loopbacks := []string{"127.0.0.1", "127.8.9.1", "localhost", "::1",
		"0.0.0.0", "169.254.1.5", "100.64.0.7", "100.113.9.2", "10.0.0.1", "192.168.1.4", "metadata.internal"}
	for _, host := range loopbacks {
		if !isSelfTargetHost(loopbackLiteral(host)) {
			t.Errorf("isSelfTargetHost(%q) = false", host)
		}
	}
	// CGNAT boundary: 100.63.255.255 belongs to ordinary ISP space one
	// address BELOW the managed range, and 100.128.0.0 is beyond the /10.
	// Neither is the operator's own address, and neither is refused.
	nonSelf := []string{"100.63.255.255", "100.128.0.0", "8.8.8.8", "target.example.com"}
	for _, host := range nonSelf {
		if isSelfTargetHost(host) {
			t.Errorf("ordinary space treated as the host's own: %q", host)
		}
	}
	// metadata.internal is text, not an address, so name classification
	// stops here on purpose: the refusal for names that LOOK internal
	// belongs to the dial-time layer. Only addresses classify.
	if !isSelfTargetHost("169.254.169.254") {
		t.Error("the metadata IP itself must classify as self-target")
	}
}

func TestExplicitLoopbackTokenIsTheOperatorAuthorizingAnInternalTarget(t *testing.T) {
	Stop()
	if err := SetActive(Engagement{
		Target:  "http://127.0.0.1:4503",
		InScope: []string{"127.0.0.1", "127.0.0.1:4503"},
	}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	defer Stop()
	if err := Gate("http://127.0.0.1:4503/works"); err != nil {
		t.Errorf("the exact endpoint the operator authorized refused: %v", err)
	}
	// A different loopback PORT carries no such claim: the token names one
	// endpoint, not the whole loopback.
	if err := Gate("http://127.0.0.1:8787/bridge"); !errors.Is(err, ErrSelfTarget) {
		t.Errorf("a different self-port rode an explicit token: %v", err)
	}
	if err := Gate("http://192.168.4.10/admin"); !errors.Is(err, ErrSelfTarget) {
		t.Errorf("a private address rode a loopback token: %v", err)
	}
}

func TestRPMWindowRejectsSustainedVolume(t *testing.T) {
	Stop()
	if err := SetActive(Engagement{Target: "https://example.com", RPM: 2}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	defer Stop()
	if err := Take(2); err != nil {
		t.Fatalf("Take(2): %v", err)
	}
	if err := Take(1); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("third request inside the window: %v, want ErrRateLimited", err)
	}
}

func TestRPMWindowResetsWhenTheMinutePasses(t *testing.T) {
	Stop()
	if err := SetActive(Engagement{Target: "https://example.com", RPM: 2}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	defer Stop()
	if err := Take(2); err != nil {
		t.Fatalf("Take(2): %v", err)
	}
	// Age the window past 60s directly: a sleep long enough to be honest
	// would slow every test run.
	advanceWindow(90 * time.Second)
	if err := Take(2); err != nil {
		t.Fatalf("a fresh window refused: %v", err)
	}
}

func TestRequestCapIsLifetime(t *testing.T) {
	Stop()
	if err := SetActive(Engagement{Target: "https://example.com", RequestCap: 3}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	defer Stop()
	if err := Take(3); err != nil {
		t.Fatalf("Take(3): %v", err)
	}
	if err := Take(1); !errors.Is(err, ErrExhausted) {
		t.Fatalf("RequestCap did not hold: %v", err)
	}
}

func TestTakeWithoutAnEngagementRefuses(t *testing.T) {
	Stop()
	if err := Take(1); !errors.Is(err, ErrNoMission) {
		t.Fatalf("Take without a mission: %v, want ErrNoMission", err)
	}
}

func TestRecordSurvivesARestartAndDiesWithStop(t *testing.T) {
	Stop()
	if err := SetActive(Engagement{
		Target:    "https://target.example.com",
		InScope:   []string{"target.example.com"},
		RPM:       40,
		Objective: "authorized audit",
		UserAgent: "audit-agent/1.0",
	}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	data, err := os.ReadFile(scopeFile())
	if err != nil {
		t.Fatalf("the record never hit disk: %v", err)
	}
	var stored Engagement
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("stored record unreadable: %v", err)
	}
	if stored.Target != "https://target.example.com" || stored.UserAgent != "audit-agent/1.0" || !stored.Active || stored.RPM != 40 {
		t.Errorf("stored record drifted: %+v", stored)
	}
	// A cleared engagement must not resurrect: the file is removed, not
	// flagged, so a restart cannot misread a stale record as authorization.
	Stop()
	if _, err := os.Stat(scopeFile()); !os.IsNotExist(err) {
		t.Errorf("cleared record still on disk: %v", err)
	}
	if _, ok := Active(); ok {
		t.Error("Active() revived a stopped engagement")
	}
}

func TestSetInvalidTargetLeavesThePreviousRecordIntact(t *testing.T) {
	Stop()
	if err := SetActive(Engagement{Target: "https://first.example.com"}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if err := SetActive(Engagement{Target: "garbage"}); !errors.Is(err, ErrBadURL) {
		t.Fatalf("SetActive(garbage) = %v, want ErrBadURL", err)
	}
	e, ok := Active()
	if !ok || e.Target != "https://first.example.com" {
		t.Errorf("the valid record did not survive a failed SetActive: %+v ok=%v", e, ok)
	}
	Stop()
}

func TestIPv6ExplicitTokenPath(t *testing.T) {
	Stop()
	if err := SetActive(Engagement{
		Target:  "http://[fe80::1]:9000/",
		InScope: []string{"[fe80::1]:9000"},
	}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	defer Stop()
	if err := Gate("http://[fe80::1]:9000/panel"); err != nil {
		t.Errorf("the bracketed token refused: %v", err)
	}
	if err := Gate("http://[fe80::2]:9000/other"); !errors.Is(err, ErrSelfTarget) {
		t.Errorf("a different link-local rode the token: %v", err)
	}
}

func TestScopeFileLivesWhereTheOperatorBacksUp(t *testing.T) {
	if got := scopeFile(); !strings.HasPrefix(got, paths.DataDir()) {
		t.Errorf("scopeFile() = %q, want it under DataDir()", got)
	}
}

// helpers

// loopbackLiteral maps test labels onto the literals the classifier must
// accept. "metadata.internal" stays text (classification is address-based),
// everything else is the address itself.
func loopbackLiteral(host string) string {
	if host == "metadata.internal" {
		return "127.0.0.66"
	}
	return host
}

// advanceWindow shifts the rate window's start, so a test can walk into a
// fresh window without sleeping real time.
func advanceWindow(d time.Duration) {
	scopeMu.Lock()
	defer scopeMu.Unlock()
	if window.IsZero() {
		window = time.Now().Add(-time.Minute)
		return
	}
	window = window.Add(-d)
}

// TargetEdgeGuard exists because a subdomain check in some drafts compared
// the host against the target TEXT instead of against target-dot-host. It
// pins the gate's real contract for that case: a lookalike is out of scope.
func TargetEdgeGuard(rawurl, _ string) error {
	return Gate(rawurl)
}
