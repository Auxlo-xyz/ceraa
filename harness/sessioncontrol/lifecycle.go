package sessioncontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ceraa/harness/sessionstore"
)

var (
	ErrRequestConflict = errors.New("request id conflicts with an existing request")
	ErrParentRunStale  = errors.New("parent run is stale")
	ErrNoLauncher      = errors.New("session launcher is required")
)

// RequestEnvelope identifies one client operation. RequestID is scoped to
// ClientID, and the pair is durable in the session store.
type RequestEnvelope struct {
	ClientID  string `json:"client_id"`
	RequestID string `json:"request_id"`
}

type StartRequest struct {
	RequestEnvelope
	SessionID SessionID
	User      string
}

type FollowUpRequest struct {
	RequestEnvelope
	SessionID   SessionID
	ParentRunID RunID
	User        string
}

type ResumeRequest struct {
	RequestEnvelope
	SessionID   SessionID
	ParentRunID RunID
}

type LaunchRequest struct {
	SessionID   SessionID
	ParentRunID RunID
	User        string
	Resume      bool
	AttemptID   AttemptID
	Envelope    RequestEnvelope
}

type LaunchResult struct {
	RunID RunID
}

// Launch is the explicit seam between session control and coordinator
// execution. The coordinator remains the owner of runs and messages.
type LaunchFunc func(context.Context, LaunchRequest) (LaunchResult, error)

type StartResult struct {
	SessionID SessionID
	RunID     RunID
	Replayed  bool
}
type FollowUpResult = StartResult
type ResumeResult = StartResult

func (s *service) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	return s.launch(ctx, req.SessionID, req.RequestEnvelope, "start", "", req.User, false)
}
func (s *service) FollowUp(ctx context.Context, req FollowUpRequest) (FollowUpResult, error) {
	if err := s.checkParent(req.SessionID, req.ParentRunID, true); err != nil {
		return FollowUpResult{}, err
	}
	return s.launch(ctx, req.SessionID, req.RequestEnvelope, "follow_up", req.ParentRunID, req.User, false)
}
func (s *service) Resume(ctx context.Context, req ResumeRequest) (ResumeResult, error) {
	if err := s.recoverBeforeResume(req.SessionID); err != nil {
		return ResumeResult{}, err
	}
	if err := s.checkParent(req.SessionID, req.ParentRunID, false); err != nil {
		return ResumeResult{}, err
	}
	return s.launch(ctx, req.SessionID, req.RequestEnvelope, "resume", req.ParentRunID, "", true)
}

func (s *service) launch(ctx context.Context, sessionID SessionID, env RequestEnvelope, kind string, parent RunID, user string, resume bool) (StartResult, error) {
	if strings.TrimSpace(string(sessionID)) == "" || strings.TrimSpace(env.ClientID) == "" || strings.TrimSpace(env.RequestID) == "" {
		return StartResult{}, fmt.Errorf("%w: session, client id, and request id are required", ErrInvalidRequest)
	}
	if s.deps.Launch == nil {
		return StartResult{}, ErrNoLauncher
	}
	payload := LaunchRequest{SessionID: sessionID, ParentRunID: parent, User: user, Resume: resume, Envelope: env}
	b, _ := json.Marshal(payload)
	fingerprint := hash(kind + "\x00" + string(b))
	id := env.ClientID + ":" + env.RequestID
	action, replay, err := s.deps.Store.BeginExternalAction(string(sessionID), sessionstore.ExternalAction{ID: id, Kind: kind, Fingerprint: fingerprint, Status: sessionstore.ExternalActionPending, Result: map[string]any{"client_id": env.ClientID, "request_id": env.RequestID}})
	if err != nil {
		if errors.Is(err, sessionstore.ErrExternalActionConflict) {
			return StartResult{}, fmt.Errorf("%w: %v", ErrRequestConflict, err)
		}
		return StartResult{}, err
	}
	if replay {
		if action.ExternalID == "" {
			return StartResult{SessionID: sessionID, Replayed: true}, nil
		}
		return StartResult{SessionID: sessionID, RunID: RunID(action.ExternalID), Replayed: true}, nil
	}
	result, err := s.deps.Launch(ctx, payload)
	if err != nil {
		return StartResult{}, err
	}
	if err := s.deps.Store.CompleteExternalAction(string(sessionID), id, kind, fingerprint, string(result.RunID), map[string]any{"run_id": string(result.RunID)}); err != nil {
		return StartResult{}, err
	}
	return StartResult{SessionID: sessionID, RunID: result.RunID}, nil
}

func (s *service) checkParent(sessionID SessionID, parent RunID, followup bool) error {
	if strings.TrimSpace(string(parent)) == "" {
		return fmt.Errorf("%w: parent run id is required", ErrInvalidRequest)
	}
	sess, err := s.deps.Store.Load(string(sessionID))
	if err != nil {
		return err
	}
	if len(sess.Runs) == 0 || sess.Runs[len(sess.Runs)-1].ID != string(parent) {
		return fmt.Errorf("%w: %s", ErrParentRunStale, parent)
	}
	run := sess.Runs[len(sess.Runs)-1]
	if followup && run.Status != sessionstore.RunCompleted {
		return fmt.Errorf("%w: run is not completed", ErrParentRunStale)
	}
	if !followup && run.Status != sessionstore.RunInterrupted {
		return fmt.Errorf("%w: run is not interrupted", ErrParentRunStale)
	}
	return nil
}
func (s *service) recoverBeforeResume(sessionID SessionID) error {
	sess, err := s.deps.Store.Load(string(sessionID))
	if err != nil {
		return err
	}
	if sessionstore.Recover(sess) {
		return s.deps.Store.Save(sess)
	}
	return nil
}
func hash(v string) string { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:]) }
