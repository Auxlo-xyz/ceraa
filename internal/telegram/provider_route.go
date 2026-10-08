package telegram

import "strings"

// Routing for the provider:<id>... callback family, in one place.
//
// The dead button this replaces was a routing bug, not a rendering bug. A provider
// screen emitted "provider:<id>:models" and handleCallback matched
// "provider:<id>:model:<model>" and nothing else. There is no fallback for an
// unhandled callback, so the tap did nothing at all — and a no-op next to a stale
// list is the worst version of this failure, because the previous provider's models
// stay on screen looking current. That is the shape of bug where the user cannot
// tell the app is broken: they just see the wrong provider's models.
//
// Kept pure and separate from the effects so a test can assert that every button a
// provider screen renders resolves to a route, rather than reading a hand-written
// list that agrees with whatever the code happens to do today.
type providerRouteKind int

const (
	providerRouteNone providerRouteKind = iota
	providerRouteActivate
	providerRouteModels
	providerRoutePickModel
	providerRouteRelogin
)

// subscriptionProviderIDs are the providers served by their own gateway, with a
// model list of their own.
var subscriptionProviderIDs = []string{"codex", "hermes", "xai", "minimax", "cline", "opencode", "deepseek"}

// switchableProviderIDs are the ones reached by activating from the provider menu.
// Codex is absent because it has its own screen with its own activate and login
// callbacks, so "provider:codex" is not a route and must not become one.
var switchableProviderIDs = []string{"hermes", "xai", "minimax", "cline", "opencode", "deepseek"}

func isSwitchable(providerID string) bool {
	for _, id := range switchableProviderIDs {
		if id == providerID {
			return true
		}
	}
	return false
}

// routeProviderCallback resolves the provider family. value is the model id for
// providerRoutePickModel and empty otherwise.
func routeProviderCallback(data string) (providerID string, kind providerRouteKind, value string) {
	rest, ok := strings.CutPrefix(data, "provider:")
	if !ok {
		return "", providerRouteNone, ""
	}
	providerID, tail, hasTail := strings.Cut(rest, ":")
	known := false
	for _, id := range subscriptionProviderIDs {
		if id == providerID {
			known = true
			break
		}
	}
	if !known {
		return "", providerRouteNone, ""
	}
	if !hasTail {
		if !isSwitchable(providerID) {
			return "", providerRouteNone, ""
		}
		return providerID, providerRouteActivate, ""
	}
	switch {
	case tail == "models":
		return providerID, providerRouteModels, ""
	case tail == "relogin":
		if !isSwitchable(providerID) {
			return "", providerRouteNone, ""
		}
		return providerID, providerRouteRelogin, ""
	case strings.HasPrefix(tail, "model:"):
		model := strings.TrimPrefix(tail, "model:")
		if model == "" {
			return "", providerRouteNone, ""
		}
		return providerID, providerRoutePickModel, model
	}
	return "", providerRouteNone, ""
}
