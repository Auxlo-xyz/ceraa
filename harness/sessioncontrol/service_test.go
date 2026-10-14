package sessioncontrol

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"ceraa/harness/inbox"

	"ceraa/harness/permission"
	"ceraa/harness/question"
	"ceraa/harness/sessionstore"
)

func TestStatusProjectsRunsAttemptsAndEvidence(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&sessionstore.Session{ID: "s", Runs: []sessionstore.Run{{ID: "r", Status: sessionstore.RunCompleted, UpdatedAt: time.Now()}}, RunEvents: []sessionstore.RunEvent{{Seq: 2, RunID: "r", Type: sessionstore.RunEventCompleted}}, Attempts: []sessionstore.Attempt{{ID: "a", RunID: "r", Kind: "test", Outcome: sessionstore.OutcomeSucceeded, Evidence: []sessionstore.EvidenceRef{{ID: "e", Kind: "log"}}}}}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	status, err := svc.Status(context.Background(), StatusRequest{SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != StatusCompleted || status.LatestEventSeq != 2 || len(status.Attempts) != 1 || len(status.Evidence) != 1 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestCancelTargetsExactRunningRunAndDeduplicatesRequest(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&sessionstore.Session{ID: "s", Runs: []sessionstore.Run{{ID: "run-1", Status: sessionstore.RunRunning}}}); err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 16)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Dependencies{Store: store, Inbox: func(id string) (*inbox.Inbox, bool) { return ib, id == "s" }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := ControlContext{Actor: Actor{ID: "owner", Source: "miniapp"}}
	first, err := svc.Cancel(context.Background(), CancelRequest{SessionID: "s", RunID: "run-1", RequestID: "cancel-1", Reason: "user", Context: ctx})
	if err != nil || !first.Accepted || first.RunID != "run-1" {
		t.Fatalf("first cancel = %+v, err = %v", first, err)
	}
	second, err := svc.Cancel(context.Background(), CancelRequest{SessionID: "s", RunID: "run-1", RequestID: "cancel-1", Reason: "retry", Context: ctx})
	if err != nil || !second.Accepted {
		t.Fatalf("duplicate cancel = %+v, err = %v", second, err)
	}
	time.Sleep(10 * time.Millisecond)
	items := ib.Drain()
	if len(items) != 1 || string(items[0].ID) != "cancel-1" {
		t.Fatalf("queued cancellations = %+v, want one exact request", items)
	}
	if _, err := svc.Cancel(context.Background(), CancelRequest{SessionID: "s", RunID: "old-run", RequestID: "cancel-2", Context: ctx}); !errors.Is(err, ErrDecisionConflict) {
		t.Fatalf("wrong-run cancellation error = %v, want ErrDecisionConflict", err)
	}
	if items := ib.Drain(); len(items) != 0 {
		t.Fatalf("wrong-run cancellation queued %d requests", len(items))
	}
}
func TestCancelRejectsWrongActorAndSession(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&sessionstore.Session{ID: "owned", Runs: []sessionstore.Run{{ID: "run-1", Status: sessionstore.RunRunning}}}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Dependencies{
		Store: store,
		Authorize: func(sessionID SessionID, ctx ControlContext) bool {
			return sessionID == "owned" && ctx.Actor == (Actor{ID: "owner", Source: "miniapp"})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]CancelRequest{
		"wrong actor":   {SessionID: "owned", RunID: "run-1", RequestID: "cancel-actor", Context: ControlContext{Actor: Actor{ID: "other", Source: "miniapp"}}},
		"wrong session": {SessionID: "other", RunID: "run-1", RequestID: "cancel-session", Context: ControlContext{Actor: Actor{ID: "owner", Source: "miniapp"}}},
		"wrong run":     {SessionID: "owned", RunID: "old-run", RequestID: "cancel-run", Context: ControlContext{Actor: Actor{ID: "owner", Source: "miniapp"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Cancel(context.Background(), req); !errors.Is(err, ErrInvalidRequest) && !errors.Is(err, ErrDecisionConflict) {
				t.Fatalf("cancel error = %v, want authorization or exact-run conflict", err)
			}
		})
	}
}

func TestCancelDuplicateRequestsQueueOnceUnderConcurrency(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&sessionstore.Session{ID: "s", Runs: []sessionstore.Run{{ID: "run-1", Status: sessionstore.RunRunning}}}); err != nil {
		t.Fatal(err)
	}
	ib, err := inbox.New(context.Background(), nil, 64)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Dependencies{Store: store, Inbox: func(id string) (*inbox.Inbox, bool) { return ib, id == "s" }})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, callErr := svc.Cancel(context.Background(), CancelRequest{
				SessionID: "s", RunID: "run-1", RequestID: "cancel-once",
				Context: ControlContext{Actor: Actor{ID: "owner", Source: "miniapp"}},
			})
			errs <- callErr
		}()
	}
	wg.Wait()
	close(errs)
	for callErr := range errs {
		if callErr != nil {
			t.Fatalf("concurrent cancel = %v", callErr)
		}
	}
	items := ib.Drain()
	if len(items) != 1 || string(items[0].ID) != "cancel-once" {
		t.Fatalf("queued cancellations = %+v, want one exact request", items)
	}
}

func TestDecisionControlAnswersAndRejectsDuplicates(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&sessionstore.Session{ID: "s", Runs: []sessionstore.Run{{ID: "r", Status: sessionstore.RunRunning}}}); err != nil {
		t.Fatal(err)
	}
	permissions := permission.NewManager()
	questions := question.NewManager()
	svc, err := New(Dependencies{Store: store, Permission: permissions, Question: questions})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = permissions.Create(context.Background(), "s", "p", "bash", time.Second, nil) }()
	deadline := time.Now().Add(time.Second)
	for !permissions.IsPending("s", "p") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got, err := svc.AnswerPermission(context.Background(), AnswerPermissionRequest{SessionID: "s", RunID: "r", RequestID: "p", Decision: "allowOnce", Context: ControlContext{Actor: Actor{ID: "test", Source: "test"}}})
	if err != nil || !got.Accepted {
		t.Fatalf("answer = %+v, err = %v", got, err)
	}
	if _, err := svc.AnswerPermission(context.Background(), AnswerPermissionRequest{SessionID: "s", RunID: "r", RequestID: "p", Decision: "deny", Context: ControlContext{Actor: Actor{ID: "test", Source: "test"}}}); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := svc.AnswerQuestion(context.Background(), AnswerQuestionRequest{SessionID: "s", RunID: "other", RequestID: "q", Answer: "yes", Context: ControlContext{Actor: Actor{ID: "test", Source: "test"}}}); !errors.Is(err, ErrDecisionConflict) {
		t.Fatalf("run conflict = %v", err)
	}
}
