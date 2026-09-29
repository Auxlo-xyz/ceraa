package telegram

import "ceraa/internal/logger"

// A sign-in that completes in the background must not choose the provider.
//
// All five OAuth providers record an owner for a device flow and, on completion,
// activate that provider when the sign-in asked to switch. The flow is
// asynchronous: it can finish minutes later, after the person has switched to a
// different provider on purpose. Activating then overwrites that choice, and the
// symptom is baffling from the outside -- the switch reports success, the next
// hub still names the old provider, and a restart appears to fix it.
//
// It was caught by a log line reading "minimax activate: provider=xai
// model=MiniMax-M3": the MiniMax activation ran while xAI was active, which is
// only possible if it was decided before the xAI switch and applied after it.
//
// So the flow records which provider was active when it started, and only
// activates if that is still the case. A sign-in that the person has since
// overridden signs in and stops there, which is what they asked for by switching.

// flowActivationStillWanted reports whether a completed sign-in should still change
// the active provider.
//
// activate is whether this sign-in asked to switch at all. activeAtStart is the
// provider that was active when the flow began; an empty value means the flow was
// not tracked closely enough to know, and the conservative reading is to activate,
// because that is the pre-existing behaviour and a sign-in that asked to switch
// should not be silently ignored.
func flowActivationStillWanted(providerName string, activate bool, activeAtStart string) bool {
	if !activate {
		return false
	}
	if activeAtStart == "" {
		return true
	}
	current := defaultState.provider()
	if current == activeAtStart {
		return true
	}
	logger.Info("provider sign-in completed but not switching: " + providerName +
		" sign-in started while " + activeAtStart + " was active, and " + current +
		" has been chosen since. Sign-in stored; leaving the active provider alone.")
	return false
}

// flowNotActiveReason is the sentence a chat sees instead of a switch it did not
// get. It separates the two reasons, because they are not the same complaint: a
// sign-in that never asked to switch is a status report, whereas one whose switch
// was superseded needs to say so, or the person is left believing the tap landed
// and was simply ignored.
func flowNotActiveReason(providerName string, activate bool) string {
	if !activate {
		return providerName + " is signed in but not the active provider, because this sign-in did not ask to switch."
	}
	return providerName + " is signed in, but another provider was chosen while the sign-in was running, " +
		"so it did not take over. The credential is stored: switch to " + providerName + " when you want it."
}
