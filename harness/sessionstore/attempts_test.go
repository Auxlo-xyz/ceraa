package sessionstore

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAttemptsLinkTypedOutcomeAndEvidenceManifest(t *testing.T) {
	sess := &Session{}
	at := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	ref := NewEvidenceRef("log-1", "test-log", "file:///tmp/test.log", "passed", at)
	if err := sess.AddAttempt(Attempt{ID: "a1", Kind: "verification", Outcome: OutcomeFailed, ActionRequired: ActionRetry, Evidence: []EvidenceRef{ref}, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AddAttempt(Attempt{ID: "a2", ParentID: "a1", Kind: "verification", Outcome: OutcomeSucceeded, Evidence: []EvidenceRef{NewEvidenceRef("log-2", "test-log", "file:///tmp/test-2.log", "passed again", at)}}); err != nil {
		t.Fatal(err)
	}
	if got := sess.Attempt("a2").ParentID; got != "a1" {
		t.Fatalf("parent id = %q", got)
	}
	if got := sess.Attempt("a2").ActionRequired; got != ActionNone {
		t.Fatalf("default action = %q", got)
	}
	manifest := sess.EvidenceManifest()
	if len(manifest) != 2 || !strings.HasPrefix(manifest[0].Digest, "sha256:") {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestAttemptsRejectDanglingOrUntypedRecords(t *testing.T) {
	sess := &Session{}
	cases := []Attempt{
		{ID: "child", ParentID: "missing", Kind: "check", Outcome: OutcomePending},
		{ID: "bad-outcome", Kind: "check", Outcome: AttemptOutcome("surprise")},
		{ID: "bad-action", Kind: "check", Outcome: OutcomePending, ActionRequired: ActionRequired("surprise")},
	}
	for _, attempt := range cases {
		if err := sess.AddAttempt(attempt); err == nil {
			t.Fatalf("accepted invalid attempt %+v", attempt)
		}
	}
}

func TestAttemptsRoundTripAsDurableManifest(t *testing.T) {
	sess := &Session{
		ID: "attempts",
		Attempts: []Attempt{{
			ID:             "a1",
			Kind:           "check",
			Outcome:        OutcomeInconclusive,
			ActionRequired: ActionReview,
			Evidence:       []EvidenceRef{{ID: "e1", Kind: "report"}},
		}},
	}
	encoded, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Session
	if err := json.Unmarshal(encoded, &loaded); err != nil {
		t.Fatal(err)
	}
	if got := loaded.Attempt("a1"); got == nil || got.Outcome != OutcomeInconclusive || got.Evidence[0].ID != "e1" {
		t.Fatalf("round trip lost attempt: %+v", loaded.Attempts)
	}
}
