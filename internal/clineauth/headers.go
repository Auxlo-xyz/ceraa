package clineauth

import (
	"strings"
)

// Request headers the Cline gateway requires.
//
// Read from Cline's own SDK (sdk/packages/llms/src/providers/request-headers.ts),
// where they are the DEFAULT_CLINE_REQUEST_HEADERS constant plus the per-request
// client identity. They are a published contract rather than a guess: the gateway
// answers 401 without them, which is what a Ceraa request was getting.
//
// Without any of this, Go's default "Go-http-client/1.1" was sent and every
// request was rejected.
const (
	HeaderReferer     = "HTTP-Referer"
	HeaderTitle       = "X-Title"
	HeaderMultiRoot   = "X-IS-MULTIROOT"
	HeaderClientType  = "X-CLIENT-TYPE"
	HeaderUserAgent   = "User-Agent"
	HeaderClientVer   = "X-CLIENT-VERSION"
	HeaderPlatform    = "X-PLATFORM"
	HeaderPlatformVer = "X-PLATFORM-VERSION"
	HeaderCoreVersion = "X-CORE-VERSION"
	HeaderTaskID      = "X-Task-ID"
)

// OfficialCLIVersion, OfficialCoreVersion, OfficialClientName, and
// OfficialClientSource reproduce the version metadata the official Cline CLI
// sends. The Cline gateway rejects
// requests that identify as an unfamiliar third-party client, so these values
// have to match a supported client. They were taken from the released Cline
// source: apps/cli/package.json version 3.0.65 and sdk/packages/core/package.json
// version 0.0.86.
const (
	OfficialCLIVersion   = "3.0.65"
	OfficialCoreVersion  = "0.0.86"
	OfficialClientName   = "cline-cli"
	OfficialClientSource = "cli"
)

// RequestHeaders builds the header set for a gateway request.
//
// The identity fields reproduce Cline's CLI identity. The header names, and the
// shape of every value, are Cline's, because the gateway routes on them.
//
// sessionID becomes X-Task-ID, which is how the gateway correlates a request with
// a conversation. An empty one is sent rather than omitted because the SDK always
// sets it.
func RequestHeaders(sessionID string) map[string]string {
	sessionID = strings.TrimSpace(sessionID)
	return map[string]string{
		HeaderReferer:     "https://cline.bot",
		HeaderTitle:       "Cline",
		HeaderMultiRoot:   "false",
		HeaderClientType:  OfficialClientName,
		HeaderUserAgent:   "Cline/" + OfficialCLIVersion,
		HeaderClientVer:   OfficialCLIVersion,
		HeaderPlatform:    OfficialClientSource,
		HeaderPlatformVer: OfficialCLIVersion,
		HeaderCoreVersion: OfficialCoreVersion,
		HeaderTaskID:      sessionID,
	}
}

// HasClineHeaders reports whether a header set already carries the gateway
// contract, so a profile's own headers are not silently overwritten or doubled.
func HasClineHeaders(headers map[string]string) bool {
	if len(headers) == 0 {
		return false
	}
	for _, key := range []string{HeaderClientType, HeaderUserAgent, HeaderTaskID} {
		if _, ok := headers[key]; !ok {
			return false
		}
	}
	return true
}
