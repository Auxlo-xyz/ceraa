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

// The identity is Ceraa's. Cline's SDK supports overriding the client name, which
// is the clearest sign a non-Cline client is expected to identify itself, so
// claiming to be their SDK would be a false statement to their server rather than
// a working integration.
func TestHeadersIdentifyCeraaNotCline(t *testing.T) {
	headers := RequestHeaders("s")
	if headers[HeaderClientType] != "ceraa" {
		t.Errorf("X-CLIENT-TYPE = %q, want ceraa", headers[HeaderClientType])
	}
	if got := headers[HeaderUserAgent]; got != "Ceraa/"+ClientVersion {
		t.Errorf("User-Agent = %q", got)
	}
	if headers[HeaderTitle] != "Ceraa" {
		t.Errorf("X-Title = %q, want Ceraa", headers[HeaderTitle])
	}
	// Nothing in the set may claim to be the official client.
	for key, value := range headers {
		if key == HeaderReferer {
			continue
		}
		if value == "cline-sdk" || value == "Cline" {
			t.Errorf("%s claims to be the official client: %q", key, value)
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
	partial := map[string]string{HeaderClientType: "ceraa"}
	if HasClineHeaders(partial) {
		t.Error("a partial set was reported as complete")
	}
}
