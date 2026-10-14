package sessionstore

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// ExternalAction is the durable receipt for one logical side effect outside
// the process, such as one Telegram send. ID is the caller's stable logical
// key, while Fingerprint identifies the exact request bound to that key.
// Result and ExternalID are intentionally provider-neutral and may contain
// only replay-safe, non-secret data.
type ExternalAction struct {
	ID          string               `json:"id"`
	Kind        string               `json:"kind"`
	Fingerprint string               `json:"fingerprint"`
	Status      ExternalActionStatus `json:"status"`
	ExternalID  string               `json:"external_id,omitempty"`
	Result      map[string]any       `json:"result,omitempty"`
	Error       string               `json:"error,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
}

type ExternalActionStatus string

const (
	ExternalActionPending   ExternalActionStatus = "pending"
	ExternalActionCompleted ExternalActionStatus = "completed"
	ExternalActionFailed    ExternalActionStatus = "failed"
)

var (
	ErrExternalActionConflict = errors.New("external action conflicts with existing request")
	ErrExternalActionNotFound = errors.New("external action not found")
)

// BeginExternalAction durably records an action before its side effect runs.
// A matching existing record is returned unchanged, so callers can replay a
// completed result or decide how to resume a pending/failed attempt. A reused
// ID with a different kind or fingerprint is always a conflict.
func (s *Store) BeginExternalAction(sessionID string, action ExternalAction) (existing ExternalAction, replay bool, err error) {
	if err := validateExternalAction(action); err != nil {
		return ExternalAction{}, false, err
	}
	err = s.Update(sessionID, func(sess *Session) error {
		for _, current := range sess.ExternalActions {
			if current.ID != action.ID {
				continue
			}
			if current.Kind != action.Kind || current.Fingerprint != action.Fingerprint {
				return fmt.Errorf("%w: id %q", ErrExternalActionConflict, action.ID)
			}
			existing = cloneExternalAction(current)
			replay = true
			return nil
		}
		now := time.Now()
		if action.CreatedAt.IsZero() {
			action.CreatedAt = now
		}
		if action.UpdatedAt.IsZero() {
			action.UpdatedAt = action.CreatedAt
		}
		if action.Status == "" {
			action.Status = ExternalActionPending
		}
		sess.ExternalActions = append(sess.ExternalActions, cloneExternalAction(action))
		existing = cloneExternalAction(action)
		return nil
	})
	return existing, replay, err
}

// CompleteExternalAction records the provider result. Repeating completion
// with the same receipt is harmless; changing the receipt is a conflict.
func (s *Store) CompleteExternalAction(sessionID, id, kind, fingerprint, externalID string, result map[string]any) error {
	return s.Update(sessionID, func(sess *Session) error {
		for i := range sess.ExternalActions {
			action := &sess.ExternalActions[i]
			if action.ID != id {
				continue
			}
			if action.Kind != kind || action.Fingerprint != fingerprint {
				return fmt.Errorf("%w: id %q", ErrExternalActionConflict, id)
			}
			if action.Status == ExternalActionCompleted {
				if action.ExternalID != externalID || !sameResult(action.Result, result) {
					return fmt.Errorf("%w: completion for id %q differs", ErrExternalActionConflict, id)
				}
				return nil
			}
			action.Status = ExternalActionCompleted
			action.ExternalID = externalID
			action.Result = cloneMap(result)
			action.Error = ""
			action.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("%w: %q", ErrExternalActionNotFound, id)
	})
}

// ExternalAction returns a copy of a receipt, suitable for replay.
func (s *Session) ExternalAction(id string) (ExternalAction, bool) {
	if s == nil {
		return ExternalAction{}, false
	}
	for _, action := range s.ExternalActions {
		if action.ID == id {
			return cloneExternalAction(action), true
		}
	}
	return ExternalAction{}, false
}

func validateExternalAction(action ExternalAction) error {
	if strings.TrimSpace(action.ID) == "" || strings.TrimSpace(action.Kind) == "" || strings.TrimSpace(action.Fingerprint) == "" {
		return errors.New("external action id, kind, and fingerprint are required")
	}
	if action.Status != "" && action.Status != ExternalActionPending {
		return fmt.Errorf("invalid initial external action status %q", action.Status)
	}
	return nil
}

func cloneExternalAction(action ExternalAction) ExternalAction {
	action.Result = cloneMap(action.Result)
	return action
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

func sameResult(left, right map[string]any) bool {
	return reflect.DeepEqual(left, right)
}
