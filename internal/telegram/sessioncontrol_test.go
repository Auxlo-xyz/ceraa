package telegram

import (
	"testing"
	"time"

	"ceraa/harness/question"
	"ceraa/harness/sessionstore"
)

// The question card answers through the shared control service, whose default
// gate accepts only requests carrying a valid actor. The bridge used to send
// the zero context, so every option tap was refused with "Question already
// answered or expired" while the question sat unanswered on screen.
func TestAQuestionButtonAnswerLandsUnderTheTelegramActor(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&sessionstore.Session{
		ID:   "tg-target",
		Runs: []sessionstore.Run{{ID: "run-1", Status: sessionstore.RunRunning}},
	}); err != nil {
		t.Fatal(err)
	}

	const requestID = "req-actor"
	answered := make(chan error, 1)
	go func() {
		_, askErr := question.Default.Create("tg-target", requestID, "how do you want to finish?", []string{"a", "b"}, 10*time.Second)
		answered <- askErr
	}()
	for !question.Default.IsPending("tg-target", requestID) {
		time.Sleep(5 * time.Millisecond)
	}

	if !answerTelegramQuestion(store, "tg-target", requestID, "a", 7598252261) {
		t.Fatal("the answer was refused, which is the failure this seam had in production")
	}
	if err := <-answered; err != nil {
		t.Fatalf("the waiting call errored on the answer: %v", err)
	}
	// The pending entry is removed by the waiter's defer, just after Create
	// returns, so give it the moment it needs before asserting.
	for i := 0; i < 100 && question.Default.IsPending("tg-target", requestID); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if question.Default.IsPending("tg-target", requestID) {
		t.Error("the question is still pending after the answer")
	}
}

// An answer the session has no active run for is exactly the dead-button case;
// the caller says the card is stale instead of pretending the answer landed.
func TestAQuestionButtonAnswerOnAnIdleSessionIsRefused(t *testing.T) {
	dir := t.TempDir()
	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&sessionstore.Session{ID: "tg-idle"}); err != nil {
		t.Fatal(err)
	}
	if answerTelegramQuestion(store, "tg-idle", "req-x", "a", 1) {
		t.Error("an answer on a session with no active run was accepted")
	}
}
