package skills

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProposalApprovalExplicitlyCreatesVersion(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddVersion("review", "version one"); err != nil {
		t.Fatal(err)
	}
	proposal, err := store.Propose("review", "version two", "include rollback guidance")
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := store.ListProposals()
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 || proposals[0].Status != Pending || proposals[0].Feedback == "" {
		t.Fatalf("unexpected proposals: %+v", proposals)
	}
	current, err := store.Current("review")
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Versions) != 1 || current.Versions[0].Content != "version one" {
		t.Fatalf("proposal changed skill before approval: %+v", current)
	}
	approved, err := store.Approve(proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(approved.Versions) != 2 || approved.Versions[1].Version != 2 || approved.Versions[1].Content != "version two" {
		t.Fatalf("approval did not append a version: %+v", approved)
	}
	rolledBack, err := store.Rollback("review", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rolledBack.Versions) != 3 || rolledBack.Versions[2].Content != "version one" {
		t.Fatalf("rollback did not append a restored version: %+v", rolledBack)
	}
	if _, err := store.Approve(proposal.ID); !errors.Is(err, ErrProposalNotPending) {
		t.Fatalf("expected non-pending approval error, got %v", err)
	}

	rejected, err := store.Propose("review", "version three", "add more examples")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Reject(rejected.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Reject(rejected.ID); !errors.Is(err, ErrProposalNotPending) {
		t.Fatalf("expected non-pending rejection error, got %v", err)
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	current, err = reopened.Current("review")
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Versions) != 3 || current.Versions[2].Content != "version one" {
		t.Fatalf("rejected proposal changed current skill: %+v", current)
	}
	proposals, err = reopened.ListProposals()
	if err != nil {
		t.Fatal(err)
	}
	if proposals[0].Status != Approved || proposals[0].Version != 2 || proposals[1].Status != Rejected {
		t.Fatalf("proposal states were not durable: %+v", proposals)
	}
}

func TestActiveContextUsesOnlyLatestVersionsAndIsBounded(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddVersion("alpha", "old alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddVersion("alpha", "current alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddVersion("beta", strings.Repeat("large skill content ", 100)); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Propose("alpha", "unapproved alpha", "improve the workflow")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != Pending {
		t.Fatalf("unexpected proposal status: %s", pending.Status)
	}
	context, err := store.ActiveContext(240)
	if err != nil {
		t.Fatal(err)
	}
	if len(context) > 240 || !utf8.ValidString(context) {
		t.Fatalf("active context is not byte bounded: bytes=%d context=%q", len(context), context)
	}
	if !strings.Contains(context, "current alpha") || strings.Contains(context, "old alpha") || strings.Contains(context, "unapproved alpha") {
		t.Fatalf("active context selected the wrong content: %q", context)
	}
	if strings.Contains(context, "large skill content") {
		t.Fatalf("active context exceeded its compact budget: %q", context)
	}
	short, err := store.ActiveContext(29)
	if err != nil {
		t.Fatal(err)
	}
	if len(short) > 29 || !utf8.ValidString(short) {
		t.Fatalf("short active context is not byte bounded: bytes=%d context=%q", len(short), short)
	}
}

func TestWritesLeaveValidJSONAndNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := store.Propose("workflow", "approved content", "make this reusable")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(proposal.ID); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("skills store is not valid JSON: %q", data)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".skills.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary skills files remain: %v", matches)
	}
}
