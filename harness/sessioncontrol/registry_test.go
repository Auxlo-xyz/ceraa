package sessioncontrol

import (
	"errors"
	"sync"
	"testing"

	"ceraa/harness/operation"
	"ceraa/harness/permission"
	"ceraa/harness/question"
)

func TestRegistryRegisterGetListAndUnregister(t *testing.T) {
	registry := NewRegistry()
	operations := operation.New(nil)
	permissions := permission.NewManager()
	questions := question.NewManager()
	first := NewRunHandle("session-b", "run-b", nil, operations, permissions, questions)
	second := NewRunHandle("session-a", "run-a", nil, operations, nil, nil)
	if err := registry.Register(first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(second); err != nil {
		t.Fatal(err)
	}
	got, ok := registry.Get("session-b")
	if !ok || got != first {
		t.Fatalf("Get returned (%p, %v), want first handle", got, ok)
	}
	if got.Operations != operations || got.Permission != permissions || got.Question != questions {
		t.Fatal("registry did not preserve owned dependencies")
	}
	listed := registry.List()
	if len(listed) != 2 || listed[0] != second || listed[1] != first {
		t.Fatalf("List returned %+v, want session order", listed)
	}
	if first.IsDone() {
		t.Fatal("new run is done")
	}
	first.MarkDone()
	first.MarkDone()
	if !first.IsDone() {
		t.Fatal("MarkDone did not close Done")
	}
	if err := registry.Unregister(first); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Get(first.SessionID); ok {
		t.Fatal("unregistered run is still active")
	}
}

func TestRegistryRejectsDuplicateAndStaleHandles(t *testing.T) {
	registry := NewRegistry()
	first := NewRunHandle("session", "run-1", nil, nil, nil, nil)
	replacement := NewRunHandle("session", "run-2", nil, nil, nil, nil)
	if err := registry.Register(first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewRunHandle("session", "run-other", nil, nil, nil, nil)); !errors.Is(err, ErrDuplicateRun) {
		t.Fatalf("duplicate registration error = %v, want ErrDuplicateRun", err)
	}
	if err := registry.Register(first); !errors.Is(err, ErrDuplicateRun) {
		t.Fatalf("same handle registration error = %v, want ErrDuplicateRun", err)
	}
	if err := registry.Unregister(replacement); !errors.Is(err, ErrStaleHandle) {
		t.Fatalf("unowned unregister error = %v, want ErrStaleHandle", err)
	}
	if err := registry.Unregister(first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Unregister(first); !errors.Is(err, ErrStaleHandle) {
		t.Fatalf("second unregister error = %v, want ErrStaleHandle", err)
	}
	if err := registry.Register(replacement); err != nil {
		t.Fatal(err)
	}
	if err := registry.Unregister(first); !errors.Is(err, ErrStaleHandle) {
		t.Fatalf("stale replacement unregister error = %v, want ErrStaleHandle", err)
	}
	got, ok := registry.Get("session")
	if !ok || got != replacement {
		t.Fatalf("replacement was not retained: (%p, %v)", got, ok)
	}
}

func TestRegistryRunLookupActorBindingAndCancellationEligibility(t *testing.T) {
	registry := NewRegistry()
	handle := NewRunHandle("session", "run-1", nil, nil, nil, nil)
	actor := Actor{ID: "actor-1", Source: "test"}
	if err := registry.Register(handle); err != nil {
		t.Fatal(err)
	}
	if got, ok := registry.LookupRun("session", "run-other"); ok || got != nil {
		t.Fatal("LookupRun returned a stale run")
	}
	if err := registry.BindActor(handle, actor); err != nil {
		t.Fatal(err)
	}
	if got := registry.Actor(handle); got != actor {
		t.Fatalf("Actor() = %+v, want %+v", got, actor)
	}
	if err := registry.BindActor(handle, actor); err != nil {
		t.Fatal("same actor should bind idempotently:", err)
	}
	if err := registry.BindActor(handle, Actor{ID: "other"}); !errors.Is(err, ErrActorConflict) {
		t.Fatalf("different actor binding error = %v, want ErrActorConflict", err)
	}
	if got, ok := registry.CancellationEligible("session", "run-1", actor); !ok || got != handle {
		t.Fatal("current actor was not eligible to cancel")
	}
	if _, ok := registry.CancellationEligible("session", "run-1", Actor{ID: "other"}); ok {
		t.Fatal("different actor was eligible to cancel")
	}
	handle.MarkDone()
	if _, ok := registry.CancellationEligible("session", "run-1", actor); ok {
		t.Fatal("finished run was eligible to cancel")
	}
}

func TestRegistryRejectsStaleActorBinding(t *testing.T) {
	registry := NewRegistry()
	first := NewRunHandle("session", "run-1", nil, nil, nil, nil)
	if err := registry.Register(first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Unregister(first); err != nil {
		t.Fatal(err)
	}
	if err := registry.BindActor(first, Actor{ID: "actor"}); !errors.Is(err, ErrStaleHandle) {
		t.Fatalf("stale binding error = %v, want ErrStaleHandle", err)
	}
	if got, ok := registry.LookupRun("session", "run-1"); ok || got != nil {
		t.Fatal("LookupRun found an unregistered run")
	}
}
func TestRegistryRejectsStaleHandleAfterReplacement(t *testing.T) {
	registry := NewRegistry()
	first := NewRunHandle("session", "run-1", nil, nil, nil, nil)
	replacement := NewRunHandle("session", "run-2", nil, nil, nil, nil)
	if err := registry.Register(first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Unregister(first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(replacement); err != nil {
		t.Fatal(err)
	}
	if got, ok := registry.LookupRun("session", "run-1"); ok || got != nil {
		t.Fatal("stale run lookup succeeded")
	}
	if got, ok := registry.CancellationEligible("session", "run-1", Actor{ID: "actor", Source: "test"}); ok || got != nil {
		t.Fatal("stale run was cancellation eligible")
	}
	if err := registry.Unregister(first); !errors.Is(err, ErrStaleHandle) {
		t.Fatalf("stale unregister = %v, want ErrStaleHandle", err)
	}
}

func TestRegistryCancellationRejectsWrongActorAndExactRun(t *testing.T) {
	registry := NewRegistry()
	handle := NewRunHandle("session", "run-1", nil, nil, nil, nil)
	owner := Actor{ID: "owner", Source: "telegram"}
	if err := registry.Register(handle); err != nil {
		t.Fatal(err)
	}
	if err := registry.BindActor(handle, owner); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		run   RunID
		actor Actor
	}{
		"wrong actor": {run: "run-1", actor: Actor{ID: "other", Source: "telegram"}},
		"wrong run":   {run: "run-old", actor: owner},
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := registry.CancellationEligible("session", test.run, test.actor); ok || got != nil {
				t.Fatal("unexpected cancellation eligibility")
			}
		})
	}
	if got, ok := registry.CancellationEligible("session", "run-1", owner); !ok || got != handle {
		t.Fatal("owner was not eligible for exact active run")
	}
}

func TestRegistryConcurrentTerminalCleanupIsStaleSafe(t *testing.T) {
	registry := NewRegistry()
	handle := NewRunHandle("session", "run-1", nil, nil, nil, nil)
	if err := registry.Register(handle); err != nil {
		t.Fatal(err)
	}
	const workers = 32
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); handle.MarkDone(); results <- registry.Unregister(handle) }()
	}
	wg.Wait()
	close(results)
	var success int
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrStaleHandle) {
			t.Fatalf("cleanup error = %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("successful cleanups = %d, want 1", success)
	}
	if _, ok := registry.Get("session"); ok {
		t.Fatal("terminal handle remained registered")
	}
}
func TestRegistryConcurrentCancelAndFinishLeavesNoActiveRun(t *testing.T) {
	registry := NewRegistry()
	handle := NewRunHandle("session", "run-1", nil, nil, nil, nil)
	owner := Actor{ID: "owner", Source: "miniapp"}
	if err := registry.Register(handle); err != nil {
		t.Fatal(err)
	}
	if err := registry.BindActor(handle, owner); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		if _, ok := registry.CancellationEligible("session", "run-1", owner); ok {
			handle.MarkDone()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		handle.MarkDone()
		_ = registry.Unregister(handle)
	}()
	close(start)
	wg.Wait()
	if _, ok := registry.Get("session"); ok {
		t.Fatal("finished run remained active after concurrent cancellation and finish")
	}
	if _, ok := registry.CancellationEligible("session", "run-1", owner); ok {
		t.Fatal("finished run remained cancellation eligible")
	}
}

func TestRegistryRejectsInvalidHandles(t *testing.T) {
	registry := NewRegistry()

	for _, handle := range []*RunHandle{NewRunHandle("", "run", nil, nil, nil, nil), NewRunHandle("session", "", nil, nil, nil, nil), nil} {
		if err := registry.Register(handle); !errors.Is(err, ErrInvalidRun) {
			t.Fatalf("Register(%v) error = %v, want ErrInvalidRun", handle, err)
		}
	}
}
