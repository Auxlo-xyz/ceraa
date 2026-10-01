package coordinator

import (
	"errors"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// errUnansweredCall is why a recovery entry exists. It is recorded on the
// operation so a later reader can tell an interrupted call from a failed one.
var errUnansweredCall = errors.New("tool call was never answered")

// recoveryResult is the history entry for a tool call the assistant declared and
// never got an answer for.
//
// The entry has to exist at all. An assistant message that declares a tool call
// with no following result is a malformed request, and providers reject it, so
// leaving one in the history does not merely lose the call: it breaks every
// request after it for the rest of the session.
//
// The wording is the part that matters. Recording only that the call failed
// invites the model to run it again, and repeating a command whose outcome is
// unknown is how a file gets written twice or a deploy happens twice. So it
// separates the two cases, and says what is safe:
//
//   - A call that never started cannot have taken effect, so retrying is free.
//   - A call that may have run is only safe to repeat when it could not have had
//     an effect, which is what the read-only set is for. Everything else is told
//     to check the state or ask first.
func recoveryResult(name string, started bool) string {
	if !started {
		return "Error: the tool call was interrupted before it was started. " +
			"Retry it if it is still needed."
	}
	if tool.ReadOnly(name) {
		return "Error: the call was interrupted and its outcome is unknown. " +
			"This tool only reads, so running it again cannot change anything."
	}
	return "Error: the call was interrupted and its outcome is unknown. " +
		"It may already have taken effect. Do not repeat it blindly: check the " +
		"affected state first, or ask before trying again."
}

// callStarted reports whether the operation record says the call got under way.
//
// A missing record counts as not started, which is the safe direction: an
// operation nobody recorded beginning is one whose effect nobody has to undo.
func callStarted(sess *sessionstore.Session, id string) bool {
	for i := range sess.Operations {
		if sess.Operations[i].ID == id {
			return sess.Operations[i].Status != sessionstore.StatusPending
		}
	}
	return false
}

// recoveryMessages builds an entry for every declared tool call that has no
// result in the history, and none for the calls that already have one.
func recoveryMessages(sess *sessionstore.Session, calls []llm.StreamedToolCall) []sessionstore.Message {
	answered := make(map[string]bool, len(calls))
	for _, m := range sess.Messages {
		if m.Role == llm.RoleTool && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	var out []sessionstore.Message
	for _, call := range calls {
		if answered[call.ID] {
			continue
		}
		out = append(out, sessionstore.Message{
			Role:              llm.RoleTool,
			ToolCallID:        call.ID,
			ToolName:          call.Name,
			ToolResult:        recoveryResult(call.Name, callStarted(sess, call.ID)),
			ToolResultPresent: true,
			ResultStatus:      r0Status(sess, call.ID),
		})
	}
	return out
}

func r0Status(sess *sessionstore.Session, id string) sessionstore.OperationStatus {
	for i := range sess.Operations {
		if sess.Operations[i].ID == id {
			return sess.Operations[i].Status
		}
	}
	return sessionstore.StatusUnknown
}

// recoverUnanswered appends a recovery entry for every tool call the assistant
// declared and never answered.
//
// It is for the paths that abandon a turn after the assistant message has been
// persisted. Those are the only way a call goes unanswered: every other exit
// either answers each call or fills in the ones still pending before returning.
//
// The append happens even when the operation record cannot be written, because the
// next request is built from the in-memory history. A malformed history is worse
// than a missing one, and the caller's own error is what gets reported.
func (r *Runner) recoverUnanswered(sess *sessionstore.Session, runID string, calls []llm.StreamedToolCall) {
	for _, m := range recoveryMessages(sess, calls) {
		// Best effort. persistUnknown saves the session itself, so it is called
		// before the append so the durable record carries the recovery too.
		_ = r.persistUnknown(sess, runID, m.ToolCallID, errUnansweredCall)
		sess.Messages = append(sess.Messages, m)
	}
}
