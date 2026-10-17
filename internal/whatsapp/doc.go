// Package whatsapp connects Ceraa to a chat account for messaging.
//
// The session database lives under the Ceraa data dir (see internal/paths)
// and pairing is done out of band by the onboarding flow through
// PairingClient. The transport stays idle unless CERAA_WHATSAPP_ENABLED
// is true; Connect refuses otherwise.
package whatsapp
