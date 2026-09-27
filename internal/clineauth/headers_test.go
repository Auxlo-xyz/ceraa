package clineauth

import "testing"

// Every header the gateway contract names has to be present. One missing produced
// the 401 that started this, so the set is pinned rather than built ad hoc.
func TestRequestHeadersCarryTheWholeContract(t *testing.T) {
	headers := RequestHeaders("sess-1")
	for _, key := range []string{
		HeaderReferer, HeaderTitle, HeaderMultiRoot, HeaderClientType,
		HeaderUserAgent, HeaderClientVer, HeaderPlatform, HeaderPlatformVer,
		HeaderCoreVersion, HeaderTaskID,
	} {
		if _, ok := headers[key]; !ok {
			t.Errorf("missing %s; the gateway rejects the request without it", key)
		}
	}
	if headers[HeaderReferer] != "https://cline.bot" {
		t.Errorf("HTTP-Referer = %q", headers[HeaderReferer])
	}
	if headers[HeaderMultiRoot] != "false" {
		t.Errorf("X-IS-MULTIROOT = %q, want the string false", headers[HeaderMultiRoot])
	}
	// X-Task-ID is how the gateway correlates a request, and the SDK always sets
	// it, so an empty session is sent rather than the header dropped.
	if headers[HeaderTaskID] != "sess-1" {
		t.Errorf("X-Task-ID = %q", headers[HeaderTaskID])
	}
	if got := RequestHeaders("  ")[""+"X-Task-ID"]; got != "" {
		t.Errorf("a blank session was not trimmed: %q", got)
	}
}

// The gateway expects Cline's own CLI identity. Ceraa values produced a 401,
// so the request must send the official shape rather than identifying this
// process.
func TestHeadersMatchOfficialCLI(t *testing.T) {
	headers := RequestHeaders("sess-1")
	want := map[string]string{
		HeaderReferer:     "https://cline.bot",
		HeaderTitle:       "Cline",
		HeaderMultiRoot:   "false",
		HeaderClientType:  "cline-cli",
		HeaderUserAgent:   "Cline/3.0.65",
		HeaderClientVer:   "3.0.65",
		HeaderPlatform:    "cli",
		HeaderPlatformVer: "3.0.65",
		HeaderCoreVersion: "0.0.86",
		HeaderTaskID:      "sess-1",
	}
	for key, expected := range want {
		if headers[key] != expected {
			t.Errorf("%s = %q, want %q", key, headers[key], expected)
		}
	}
}

// A profile's deliberate headers must not be overwritten, and a partial set is
// completed rather than trusted.
func TestHasClineHeaders(t *testing.T) {
	if HasClineHeaders(nil) {
		t.Error("nil was reported as carrying the contract")
	}
	if HasClineHeaders(RequestHeaders("s")) != true {
		t.Error("a full set was not recognised")
	}
	partial := map[string]string{HeaderClientType: "cline-cli"}
	if HasClineHeaders(partial) {
		t.Error("a partial set was reported as complete")
	}
}
