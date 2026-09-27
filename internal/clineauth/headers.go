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

// ClientVersion is Ceraa's version, sent as the client identity.
//
// Ceraa's own version, not Cline's. The gateway's version floor is checked
// server-side and its rule is not published, so a Cline version number here would
// be a claim about a client we are not. If the gateway refuses anything but a
// Cline identity, that is Cline deciding which third-party clients it supports,
// and the honest answer is to ask them rather than to answer for them.
const ClientVersion = "0.1.0"

// ClientType identifies this client.
//
// Cline's SDK sets "cline-sdk" and supports overriding it with the client's own
// name, which is the clearest evidence in the header set that a non-Cline client
// is expected to identify itself.
const ClientType = "ceraa"

// Platform is where this client runs.
const Platform = "telegram"

// RequestHeaders builds the header set for a gateway request.
//
// The identity fields name Ceraa. The header names, and the shape of every value,
// are Cline's, because the gateway routes on them.
//
// sessionID becomes X-Task-ID, which is how the gateway correlates a request with
// a conversation. An empty one is sent rather than omitted because the SDK always
// sets it.
func RequestHeaders(sessionID string) map[string]string {
	sessionID = strings.TrimSpace(sessionID)
	return map[string]string{
		HeaderReferer:     "https://cline.bot",
		HeaderTitle:       "Ceraa",
		HeaderMultiRoot:   "false",
		HeaderClientType:  ClientType,
		HeaderUserAgent:   "Ceraa/" + ClientVersion,
		HeaderClientVer:   ClientVersion,
		HeaderPlatform:    Platform,
		HeaderPlatformVer: ClientVersion,
		HeaderCoreVersion: ClientVersion,
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
