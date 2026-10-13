package sessionstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Attempt is a durable record of one effort to answer or change something.
// It links retries through ParentID and records only references to evidence.
type Attempt struct {
	ID             string         `json:"id"`
	ParentID       string         `json:"parent_id,omitempty"`
	RunID          string         `json:"run_id,omitempty"`
	Kind           string         `json:"kind"`
	Outcome        AttemptOutcome `json:"outcome"`
	ActionRequired ActionRequired `json:"action_required,omitempty"`
	Evidence       []EvidenceRef  `json:"evidence,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	Summary        string         `json:"summary,omitempty"`
}

type AttemptOutcome string

const (
	OutcomePending      AttemptOutcome = "pending"
	OutcomeSucceeded    AttemptOutcome = "succeeded"
	OutcomeFailed       AttemptOutcome = "failed"
	OutcomeInconclusive AttemptOutcome = "inconclusive"
	OutcomeBlocked      AttemptOutcome = "blocked"
)

type ActionRequired string

const (
	ActionNone   ActionRequired = "none"
	ActionRetry  ActionRequired = "retry"
	ActionUser   ActionRequired = "user"
	ActionReview ActionRequired = "review"
)

// EvidenceRef is a manifest entry. Digest is optional for external evidence,
// but when Content is supplied NewEvidenceRef always derives it.
type EvidenceRef struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	URI       string    `json:"uri,omitempty"`
	Digest    string    `json:"digest,omitempty"`
	MediaType string    `json:"media_type,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func NewEvidenceRef(id, kind, uri, content string, at time.Time) EvidenceRef {
	ref := EvidenceRef{ID: id, Kind: kind, URI: uri, CreatedAt: at}
	if content != "" {
		sum := sha256.Sum256([]byte(content))
		ref.Digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	return ref
}

// AddAttempt appends an attempt after validating its typed outcome and links.
func (s *Session) AddAttempt(attempt Attempt) error {
	if s == nil {
		return fmt.Errorf("nil session")
	}
	if strings.TrimSpace(attempt.ID) == "" {
		return fmt.Errorf("attempt id is required")
	}
	if strings.TrimSpace(attempt.Kind) == "" {
		return fmt.Errorf("attempt kind is required")
	}
	if attempt.ParentID != "" && s.Attempt(attempt.ParentID) == nil {
		return fmt.Errorf("parent attempt %q not found", attempt.ParentID)
	}
	switch attempt.Outcome {
	case OutcomePending, OutcomeSucceeded, OutcomeFailed, OutcomeInconclusive, OutcomeBlocked:
	default:
		return fmt.Errorf("invalid attempt outcome %q", attempt.Outcome)
	}
	if attempt.ActionRequired == "" {
		attempt.ActionRequired = ActionNone
	}
	switch attempt.ActionRequired {
	case ActionNone, ActionRetry, ActionUser, ActionReview:
	default:
		return fmt.Errorf("invalid action required %q", attempt.ActionRequired)
	}
	if attempt.CreatedAt.IsZero() {
		attempt.CreatedAt = time.Now()
	}
	if attempt.UpdatedAt.IsZero() {
		attempt.UpdatedAt = attempt.CreatedAt
	}
	s.Attempts = append(s.Attempts, attempt)
	return nil
}

func (s *Session) Attempt(id string) *Attempt {
	for i := range s.Attempts {
		if s.Attempts[i].ID == id {
			return &s.Attempts[i]
		}
	}
	return nil
}

func (s *Session) EvidenceManifest() []EvidenceRef {
	var manifest []EvidenceRef
	for _, attempt := range s.Attempts {
		manifest = append(manifest, attempt.Evidence...)
	}
	return manifest
}
