package coordinator

import (
	"sync"
	"time"

	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
)

// sidekickState carries what tool-output delivery needs, and records what it
// delivered so the two modes can be told apart later.
//
// The store is read through a function rather than held, for the same reason the
// prompt block is: the file on disk is the source of truth, and a snapshot taken
// at construction diverges from it silently in a long-lived daemon.
type sidekickState struct {
	mu      sync.Mutex
	lookup  func() ([]memory.Entry, error)
	workDir string

	// generalDelivered makes the standing preferences fire once per run rather than
	// once per tool call.
	//
	// A general memory has no path, so it matches every operation. Injecting it
	// each time would write it into the transcript once per call - forty copies in
	// a long run, against a prompt block that carried it once - and the copies are
	// not free: every one sits in the context for the rest of the session.
	generalDelivered bool

	report memory.DeliveryReport
}

func newSidekickState(lookup func() ([]memory.Entry, error), workDir string) *sidekickState {
	return &sidekickState{lookup: lookup, workDir: workDir}
}

// annotate returns the sidekick block for one completed operation, or "" when
// nothing qualifies.
func (s *sidekickState) annotate(sess *sessionstore.Session, result operation.ResultEvent) string {
	if s == nil || s.lookup == nil {
		return ""
	}
	entries, err := s.lookup()
	if err != nil {
		// A read failure must not fail the turn, and must not look like "there is
		// no memory", which would be a different and wrong answer.
		return ""
	}
	now := time.Now()
	targets := s.targetsFor(sess, result)

	s.mu.Lock()
	defer s.mu.Unlock()

	var targeted, general []memory.Brief
	if len(targets) > 0 {
		targeted = memory.Select(entries, targets, now, memory.SidekickTargetLimit)
		s.report.TargetedInjects++
	}
	if !s.generalDelivered {
		general = memory.SelectGeneral(entries, now, memory.SidekickGeneralLimit)
		s.generalDelivered = true
		s.report.GeneralInjected = len(general)
	}
	if len(targeted) == 0 && len(general) == 0 {
		s.report.NoteSuppressed(belowGate(entries, now))
		return ""
	}
	return memory.Annotate(targets, targeted, general)
}

// belowGate counts what the gate held back, so a run that delivered nothing can be
// told apart from a run that had nothing worth delivering.
func belowGate(entries []memory.Entry, now time.Time) int {
	n := 0
	for _, entry := range entries {
		if memory.ScoreOf(entry, now) < memory.SidekickGate {
			n++
		}
	}
	return n
}

// targetsFor is every path the completed operation was about.
func (s *sidekickState) targetsFor(sess *sessionstore.Session, result operation.ResultEvent) []string {
	for _, op := range sess.Operations {
		if op.ID != result.OpID {
			continue
		}
		return s.targetsForSpec(op.SpecType, op.SpecArgs)
	}
	return nil
}

func (s *sidekickState) deliveryReport() memory.DeliveryReport {
	if s == nil {
		return memory.DeliveryReport{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.report
}
