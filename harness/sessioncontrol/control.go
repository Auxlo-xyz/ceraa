package sessioncontrol

import (
	"context"
	"fmt"
	"strings"

	"ceraa/harness/sessionstore"
)

// UnsupportedError identifies a control operation that the current owners
// cannot perform safely. Callers can use errors.Is(err, ErrUnsupported).
type UnsupportedError struct {
	Operation string
}

func (e *UnsupportedError) Error() string {
	if e == nil || e.Operation == "" {
		return ErrUnsupported.Error()
	}
	return fmt.Sprintf("%s: %s", ErrUnsupported, e.Operation)
}

func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

type RetryRequest struct {
	RequestID     string
	SessionID     SessionID
	ParentAttempt AttemptID
	// Policy must explicitly describe a coordinator-owned retry. The facade
	// never interprets it as permission to replay a tool operation.
	Policy string
}

type RetryResult struct {
	SessionID     SessionID
	ParentAttempt AttemptID
}

type PauseRequest struct {
	RequestID string
	SessionID SessionID
	Reason    string
}

type PauseResult struct {
	SessionID SessionID
	Accepted  bool
}

func (s *service) Retry(_ context.Context, req RetryRequest) (RetryResult, error) {
	if strings.TrimSpace(string(req.SessionID)) == "" || strings.TrimSpace(req.RequestID) == "" || strings.TrimSpace(string(req.ParentAttempt)) == "" {
		return RetryResult{}, fmt.Errorf("%w: session id, request id, and parent attempt are required", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.Policy) == "" {
		return RetryResult{}, fmt.Errorf("%w: retry policy is required", ErrInvalidRequest)
	}
	sess, err := s.deps.Store.Load(string(req.SessionID))
	if err != nil {
		return RetryResult{}, err
	}
	parent := sess.Attempt(string(req.ParentAttempt))
	if parent == nil {
		return RetryResult{}, fmt.Errorf("%w: parent attempt %q not found", ErrInvalidRequest, req.ParentAttempt)
	}
	if parent.ActionRequired != sessionstore.ActionRetry {
		return RetryResult{}, fmt.Errorf("%w: attempt %q is not retryable", ErrInvalidRequest, req.ParentAttempt)
	}
	// There is currently no coordinator-owned retry seam that can accept an
	// attempt identity and establish safe tool boundaries. Refusing here is
	// safer than replaying a prompt or operation with unknown side effects.
	return RetryResult{SessionID: req.SessionID, ParentAttempt: req.ParentAttempt}, &UnsupportedError{Operation: "linked attempt retry"}
}

func (s *service) Pause(_ context.Context, req PauseRequest) (PauseResult, error) {
	if strings.TrimSpace(string(req.SessionID)) == "" || strings.TrimSpace(req.RequestID) == "" {
		return PauseResult{}, fmt.Errorf("%w: session id and request id are required", ErrInvalidRequest)
	}
	return PauseResult{SessionID: req.SessionID}, &UnsupportedError{Operation: "pause without cancel"}
}
