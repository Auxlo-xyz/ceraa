// Package sessioncontrol provides a transport-neutral control facade for Ceraa
// sessions. It composes existing stores and live event sources; it does not own
// model execution or create a second session database.
package sessioncontrol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ceraa/harness/event"
	"ceraa/harness/inbox"
	"ceraa/harness/permission"
	"ceraa/harness/question"
	"ceraa/harness/sessionstore"
)

type SessionID string
type RunID string
type AttemptID string

type Status string

const (
	StatusIdle        Status = "idle"
	StatusRunning     Status = "running"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusCanceled    Status = "canceled"
	StatusInterrupted Status = "interrupted"
)

var (
	ErrInvalidRequest   = errors.New("invalid session-control request")
	ErrUnsupported      = errors.New("session-control operation is unsupported")
	ErrRunNotFound      = errors.New("run not found")
	ErrAlreadyAnswered  = errors.New("decision was already answered")
	ErrDecisionConflict = errors.New("decision conflicts with the active run")
)

type StatusRequest struct {
	SessionID SessionID
	Context   ControlContext
}

type RunSummary struct {
	ID        RunID
	Status    sessionstore.RunStatus
	Step      int
	Requests  int
	CreatedAt time.Time
	UpdatedAt time.Time
	Error     string
}

type AttemptSummary struct {
	ID             AttemptID
	ParentID       AttemptID
	RunID          RunID
	Kind           string
	Outcome        sessionstore.AttemptOutcome
	ActionRequired sessionstore.ActionRequired
	EvidenceIDs    []string
	Summary        string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type EvidenceSummary struct {
	ID        string
	AttemptID AttemptID
	Kind      string
	URI       string
	Digest    string
	MediaType string
	CreatedAt time.Time
}

type SessionStatus struct {
	SessionID      SessionID
	Title          string
	Status         Status
	CurrentRunID   RunID
	Runs           []RunSummary
	Attempts       []AttemptSummary
	Evidence       []EvidenceSummary
	LatestEventSeq int
	UpdatedAt      time.Time
}

type Event struct {
	SessionID SessionID
	RunID     RunID
	Seq       int
	Type      string
	Status    Status
	Step      int
	Data      map[string]any
	At        time.Time
}

type EventFilter struct {
	SessionID SessionID
	RunID     RunID
	After     int
	Buffer    int
}

type CancelRequest struct {
	SessionID SessionID
	RunID     RunID
	RequestID string
	Reason    string
	Hard      bool
	Context   ControlContext
}

type CancelResult struct {
	SessionID SessionID
	RunID     RunID
	Status    Status
	Accepted  bool
	At        time.Time
}

type AnswerPermissionRequest struct {
	SessionID SessionID
	RunID     RunID
	RequestID string
	Decision  string
	Context   ControlContext
}

type AnswerQuestionRequest struct {
	SessionID SessionID
	RunID     RunID
	RequestID string
	Answer    string
	Context   ControlContext
}

type PendingDecisionsRequest struct {
	SessionID SessionID
	RunID     RunID
}

type PendingDecision struct {
	SessionID SessionID
	RunID     RunID
	RequestID string
	Kind      string
}

type PendingDecisionsResult struct {
	SessionID SessionID
	RunID     RunID
	Decisions []PendingDecision
}

type AnswerResult struct {
	SessionID SessionID
	RunID     RunID
	RequestID string
	Accepted  bool
	At        time.Time
}

type AttemptInput struct {
	SessionID      SessionID
	ParentAttempt  AttemptID
	RunID          RunID
	Kind           string
	Outcome        sessionstore.AttemptOutcome
	ActionRequired sessionstore.ActionRequired
	Summary        string
}

type AttemptResult struct {
	SessionID SessionID
	AttemptID AttemptID
	Evidence  []EvidenceSummary
}

type EvidenceInput struct {
	SessionID SessionID
	AttemptID AttemptID
	ID        string
	Kind      string
	URI       string
	Content   string
	Digest    string
	MediaType string
}

type Service interface {
	Status(context.Context, StatusRequest) (SessionStatus, error)
	Events(context.Context, EventFilter) (<-chan Event, error)
	Cancel(context.Context, CancelRequest) (CancelResult, error)
	AnswerPermission(context.Context, AnswerPermissionRequest) (AnswerResult, error)
	AnswerQuestion(context.Context, AnswerQuestionRequest) (AnswerResult, error)
	PendingDecisions(context.Context, PendingDecisionsRequest) (PendingDecisionsResult, error)
	AddAttempt(context.Context, AttemptInput) (AttemptResult, error)
	AddEvidence(context.Context, EvidenceInput) (EvidenceSummary, error)
	Start(context.Context, StartRequest) (StartResult, error)
	FollowUp(context.Context, FollowUpRequest) (FollowUpResult, error)
	Resume(context.Context, ResumeRequest) (ResumeResult, error)
	Retry(context.Context, RetryRequest) (RetryResult, error)
	Pause(context.Context, PauseRequest) (PauseResult, error)
}

type Dependencies struct {
	Store      *sessionstore.Store
	Bus        *event.Bus
	Inbox      func(string) (*inbox.Inbox, bool)
	Permission *permission.Manager
	Question   *question.Manager
	Authorize  func(SessionID, ControlContext) bool
	Now        func() time.Time
	Launch     LaunchFunc
}

type service struct {
	deps       Dependencies
	answeredMu sync.Mutex
	answered   map[string]struct{}
	cancelled  map[string]struct{}
}

func New(deps Dependencies) (Service, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("%w: session store is required", ErrInvalidRequest)
	}
	if deps.Bus == nil {
		deps.Bus = event.Default
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Permission == nil {
		deps.Permission = permission.Default
	}
	if deps.Question == nil {
		deps.Question = question.Default
	}
	if deps.Authorize == nil {
		deps.Authorize = func(_ SessionID, ctx ControlContext) bool { return ctx.Actor.valid() }
	}
	return &service{deps: deps, answered: make(map[string]struct{}), cancelled: make(map[string]struct{})}, nil
}

func (s *service) authorized(sessionID SessionID, ctx ControlContext) bool {
	return ctx.Actor.valid() && s.deps.Authorize(sessionID, ctx)
}

func (s *service) Status(_ context.Context, req StatusRequest) (SessionStatus, error) {
	sess, err := s.deps.Store.Load(string(req.SessionID))
	if err != nil {
		return SessionStatus{}, err
	}
	out := SessionStatus{SessionID: req.SessionID, Title: sess.DisplayTitle(), Status: StatusIdle}
	for _, run := range sess.Runs {
		out.Runs = append(out.Runs, RunSummary{ID: RunID(run.ID), Status: run.Status, Step: run.Step, Requests: run.Requests, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, Error: run.Error})
		if run.Status == sessionstore.RunRunning {
			out.Status = StatusRunning
			out.CurrentRunID = RunID(run.ID)
		}
		if run.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = run.UpdatedAt
		}
	}
	for _, attempt := range sess.Attempts {
		summary := AttemptSummary{ID: AttemptID(attempt.ID), ParentID: AttemptID(attempt.ParentID), RunID: RunID(attempt.RunID), Kind: attempt.Kind, Outcome: attempt.Outcome, ActionRequired: attempt.ActionRequired, Summary: attempt.Summary, CreatedAt: attempt.CreatedAt, UpdatedAt: attempt.UpdatedAt}
		for _, evidence := range attempt.Evidence {
			summary.EvidenceIDs = append(summary.EvidenceIDs, evidence.ID)
			out.Evidence = append(out.Evidence, evidenceSummary(attempt.ID, evidence))
		}
		out.Attempts = append(out.Attempts, summary)
	}
	for _, runEvent := range sess.RunEvents {
		if runEvent.Seq > out.LatestEventSeq {
			out.LatestEventSeq = runEvent.Seq
		}
	}
	if len(sess.Runs) > 0 && out.Status == StatusIdle {
		switch sess.Runs[len(sess.Runs)-1].Status {
		case sessionstore.RunCompleted:
			out.Status = StatusCompleted
		case sessionstore.RunFailed:
			out.Status = StatusFailed
		case sessionstore.RunCanceled:
			out.Status = StatusCanceled
		case sessionstore.RunInterrupted:
			out.Status = StatusInterrupted
		}
	}
	return out, nil
}

func (s *service) Events(ctx context.Context, filter EventFilter) (<-chan Event, error) {
	if strings.TrimSpace(string(filter.SessionID)) == "" {
		return nil, fmt.Errorf("%w: session id is required", ErrInvalidRequest)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if filter.Buffer <= 0 {
		filter.Buffer = 64
	}
	sess, err := s.deps.Store.Load(string(filter.SessionID))
	if err != nil {
		return nil, err
	}
	out := make(chan Event, filter.Buffer)
	live := s.deps.Bus.Subscribe(string(filter.SessionID), filter.Buffer)
	go func() {
		defer close(out)
		defer s.deps.Bus.Unsubscribe(string(filter.SessionID), live)
		for _, item := range sess.ReplayRunEvents(string(filter.RunID)) {
			if item.Seq <= filter.After {
				continue
			}
			select {
			case out <- runEvent(filter.SessionID, item):
			case <-ctx.Done():
				return
			}
		}
		for {
			select {
			case item := <-live:
				if filter.RunID != "" && item.Data != nil && item.Data["run_id"] != string(filter.RunID) {
					continue
				}
				select {
				case out <- liveEvent(filter.SessionID, item):
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (s *service) AnswerPermission(_ context.Context, req AnswerPermissionRequest) (AnswerResult, error) {
	if !s.authorized(req.SessionID, req.Context) {
		return AnswerResult{}, fmt.Errorf("%w: actor is not authorized for session", ErrInvalidRequest)
	}
	return s.answer(req.SessionID, req.RunID, req.RequestID, req.Decision, "permission", func() bool {
		return s.deps.Permission.Answer(string(req.SessionID), req.RequestID, req.Decision)
	})
}

func (s *service) AnswerQuestion(_ context.Context, req AnswerQuestionRequest) (AnswerResult, error) {
	if !s.authorized(req.SessionID, req.Context) {
		return AnswerResult{}, fmt.Errorf("%w: actor is not authorized for session", ErrInvalidRequest)
	}
	return s.answer(req.SessionID, req.RunID, req.RequestID, req.Answer, "question", func() bool {
		return s.deps.Question.Answer(string(req.SessionID), req.RequestID, req.Answer)
	})
}

func (s *service) answer(sessionID SessionID, runID RunID, requestID, value, kind string, answer func() bool) (AnswerResult, error) {
	if strings.TrimSpace(string(sessionID)) == "" || strings.TrimSpace(requestID) == "" || strings.TrimSpace(value) == "" {
		return AnswerResult{}, fmt.Errorf("%w: session id, request id, and answer are required", ErrInvalidRequest)
	}
	key := kind + ":" + string(sessionID) + ":" + requestID
	s.answeredMu.Lock()
	if _, exists := s.answered[key]; exists {
		s.answeredMu.Unlock()
		return AnswerResult{}, fmt.Errorf("%w: %s %q", ErrAlreadyAnswered, kind, requestID)
	}
	s.answeredMu.Unlock()
	status, err := s.Status(context.Background(), StatusRequest{SessionID: sessionID})
	if err != nil {
		return AnswerResult{}, err
	}
	if runID == "" {
		runID = status.CurrentRunID
	}
	if runID == "" || status.CurrentRunID != runID || status.Status != StatusRunning {
		return AnswerResult{}, fmt.Errorf("%w: session %q run %q is not active", ErrDecisionConflict, sessionID, runID)
	}
	if !answer() {
		return AnswerResult{}, fmt.Errorf("%w: %s %q", ErrAlreadyAnswered, kind, requestID)
	}
	key = kind + ":" + string(sessionID) + ":" + requestID
	s.answeredMu.Lock()
	s.answered[key] = struct{}{}
	s.answeredMu.Unlock()
	return AnswerResult{SessionID: sessionID, RunID: runID, RequestID: requestID, Accepted: true, At: s.deps.Now()}, nil
}

func (s *service) PendingDecisions(_ context.Context, req PendingDecisionsRequest) (PendingDecisionsResult, error) {
	if strings.TrimSpace(string(req.SessionID)) == "" {
		return PendingDecisionsResult{}, fmt.Errorf("%w: session id is required", ErrInvalidRequest)
	}
	status, err := s.Status(context.Background(), StatusRequest{SessionID: req.SessionID})
	if err != nil {
		return PendingDecisionsResult{}, err
	}
	if req.RunID != "" && req.RunID != status.CurrentRunID {
		return PendingDecisionsResult{}, fmt.Errorf("%w: session %q run %q is not active", ErrDecisionConflict, req.SessionID, req.RunID)
	}
	result := PendingDecisionsResult{SessionID: req.SessionID, RunID: status.CurrentRunID}
	for _, id := range s.deps.Permission.Pending(string(req.SessionID)) {
		result.Decisions = append(result.Decisions, PendingDecision{SessionID: req.SessionID, RunID: result.RunID, RequestID: id, Kind: "permission"})
	}
	for _, id := range s.deps.Question.Pending(string(req.SessionID)) {
		result.Decisions = append(result.Decisions, PendingDecision{SessionID: req.SessionID, RunID: result.RunID, RequestID: id, Kind: "question"})
	}
	return result, nil
}
func (s *service) Cancel(_ context.Context, req CancelRequest) (CancelResult, error) {
	if !s.authorized(req.SessionID, req.Context) {
		return CancelResult{}, fmt.Errorf("%w: actor is not authorized for session", ErrInvalidRequest)
	}
	if strings.TrimSpace(string(req.SessionID)) == "" || strings.TrimSpace(req.RequestID) == "" {
		return CancelResult{}, fmt.Errorf("%w: session id and request id are required", ErrInvalidRequest)
	}
	status, err := s.Status(context.Background(), StatusRequest{SessionID: req.SessionID})
	if err != nil {
		return CancelResult{}, err
	}
	if req.RunID != "" && req.RunID != status.CurrentRunID {
		return CancelResult{}, fmt.Errorf("%w: session %q run %q is not active", ErrDecisionConflict, req.SessionID, req.RunID)
	}
	result := CancelResult{SessionID: req.SessionID, RunID: status.CurrentRunID, Status: status.Status, At: s.deps.Now()}
	key := string(req.SessionID) + "\x00" + string(req.RunID) + "\x00" + req.RequestID
	s.answeredMu.Lock()
	if _, exists := s.cancelled[key]; exists {
		s.answeredMu.Unlock()
		return CancelResult{SessionID: req.SessionID, RunID: status.CurrentRunID, Status: status.Status, Accepted: true, At: s.deps.Now()}, nil
	}
	s.cancelled[key] = struct{}{}
	s.answeredMu.Unlock()
	if status.Status != StatusRunning || s.deps.Inbox == nil {
		return result, nil
	}
	ib, ok := s.deps.Inbox(string(req.SessionID))
	if !ok || ib == nil {
		return result, nil
	}
	mode := inbox.ModeStopWhenIdle
	if req.Hard {
		mode = inbox.ModeStopHard
	}
	payload := fmt.Sprintf(`{"mode":%q,"reason":%q}`, mode, req.Reason)
	if err := ib.Submit(context.Background(), inbox.Submission{ID: inbox.RequestID(req.RequestID), Kind: inbox.KindControl, Payload: []byte(payload), Received: s.deps.Now()}); err != nil {
		return CancelResult{}, err
	}
	result.Accepted = true
	return result, nil
}

func (s *service) AddAttempt(_ context.Context, input AttemptInput) (AttemptResult, error) {
	if strings.TrimSpace(string(input.SessionID)) == "" || strings.TrimSpace(input.Kind) == "" {
		return AttemptResult{}, fmt.Errorf("%w: session id and kind are required", ErrInvalidRequest)
	}
	id := fmt.Sprintf("attempt-%d", s.deps.Now().UnixNano())
	attempt := sessionstore.Attempt{ID: id, ParentID: string(input.ParentAttempt), RunID: string(input.RunID), Kind: input.Kind, Outcome: input.Outcome, ActionRequired: input.ActionRequired, Summary: input.Summary, CreatedAt: s.deps.Now(), UpdatedAt: s.deps.Now()}
	if err := s.deps.Store.Update(string(input.SessionID), func(sess *sessionstore.Session) error { return sess.AddAttempt(attempt) }); err != nil {
		return AttemptResult{}, err
	}
	return AttemptResult{SessionID: input.SessionID, AttemptID: AttemptID(id)}, nil
}

func (s *service) AddEvidence(_ context.Context, input EvidenceInput) (EvidenceSummary, error) {
	if strings.TrimSpace(string(input.SessionID)) == "" || strings.TrimSpace(string(input.AttemptID)) == "" || strings.TrimSpace(input.ID) == "" || strings.TrimSpace(input.Kind) == "" {
		return EvidenceSummary{}, fmt.Errorf("%w: session, attempt, id, and kind are required", ErrInvalidRequest)
	}
	ref := sessionstore.NewEvidenceRef(input.ID, input.Kind, input.URI, input.Content, s.deps.Now())
	ref.Digest = input.Digest
	if input.Content != "" {
		ref = sessionstore.NewEvidenceRef(input.ID, input.Kind, input.URI, input.Content, s.deps.Now())
	}
	if input.MediaType != "" {
		ref.MediaType = input.MediaType
	}
	if err := s.deps.Store.Update(string(input.SessionID), func(sess *sessionstore.Session) error {
		attempt := sess.Attempt(string(input.AttemptID))
		if attempt == nil {
			return fmt.Errorf("attempt %q not found", input.AttemptID)
		}
		for _, existing := range attempt.Evidence {
			if existing.ID == ref.ID {
				if existing.Digest == ref.Digest && existing.URI == ref.URI {
					return nil
				}
				return fmt.Errorf("evidence %q conflicts with existing reference", ref.ID)
			}
		}
		attempt.Evidence = append(attempt.Evidence, ref)
		return nil
	}); err != nil {
		return EvidenceSummary{}, err
	}
	return evidenceSummary(string(input.AttemptID), ref), nil
}

func evidenceSummary(attemptID string, ref sessionstore.EvidenceRef) EvidenceSummary {
	return EvidenceSummary{ID: ref.ID, AttemptID: AttemptID(attemptID), Kind: ref.Kind, URI: ref.URI, Digest: ref.Digest, MediaType: ref.MediaType, CreatedAt: ref.CreatedAt}
}

func runEvent(sessionID SessionID, item sessionstore.RunEvent) Event {
	return Event{SessionID: sessionID, RunID: RunID(item.RunID), Seq: item.Seq, Type: item.Type, Status: runStatus(item.Status), Step: item.Step, At: item.CreatedAt}
}

func liveEvent(sessionID SessionID, item event.Event) Event {
	var runID string
	if item.Data != nil {
		runID, _ = item.Data["run_id"].(string)
	}
	return Event{SessionID: sessionID, RunID: RunID(runID), Type: string(item.Type), Data: item.Data, At: item.Time}
}

func runStatus(status sessionstore.RunStatus) Status {
	switch status {
	case sessionstore.RunRunning:
		return StatusRunning
	case sessionstore.RunCompleted:
		return StatusCompleted
	case sessionstore.RunFailed:
		return StatusFailed
	case sessionstore.RunCanceled:
		return StatusCanceled
	case sessionstore.RunInterrupted:
		return StatusInterrupted
	default:
		return StatusIdle
	}
}
