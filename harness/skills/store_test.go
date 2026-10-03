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

// The catalogue is an index, not the library: names and descriptions only, the
// latest version of each, no unapproved content, and a byte bound. Inlining the
// bodies was the previous behaviour and it put the whole skill library into
// every prompt, bounded only by a ceiling that silently ate skills.
func TestCatalogContextListsNamesWithoutBodies(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddVersion("alpha", "# Alpha\n\nUse this workflow when writing a migration.\n\nSECRET_BODY_ALPHA"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddVersion("alpha", "# Alpha\n\nThe current text of the migration workflow.\n\nSECRET_BODY_ALPHA_V2"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddVersion("beta", "# Beta\n\nCovers the other workflow.\n\n"+strings.Repeat("large skill content ", 100)); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Propose("alpha", "unapproved alpha", "improve the workflow")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != Pending {
		t.Fatalf("unexpected proposal status: %s", pending.Status)
	}

	catalog, err := store.CatalogContext(2000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(catalog, "<available_skills>") || !strings.Contains(catalog, `name="alpha"`) {
		t.Fatalf("catalog is not an index of approved skills: %q", catalog)
	}
	// The description comes from the prose, not the heading.
	if !strings.Contains(catalog, "current text of the migration workflow") {
		t.Errorf("catalog lost the description: %q", catalog)
	}
	// The body must not be in the prompt at all. This is the whole point.
	for _, body := range []string{"SECRET_BODY_ALPHA", "SECRET_BODY_ALPHA_V2", "large skill content", "unapproved alpha"} {
		if strings.Contains(catalog, body) {
			t.Errorf("catalog leaked a body (%q): %q", body, catalog)
		}
	}

	// Bounded, and valid UTF-8 at the bound.
	bounded, err := store.CatalogContext(120)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) > 120 || !utf8.ValidString(bounded) {
		t.Fatalf("catalog is not byte bounded: bytes=%d catalog=%q", len(bounded), bounded)
	}
}

// Describe is a heuristic over markdown, so each fallback it can take is
// pinned: prose is preferred, a heading is the fallback, and a code fence or a
// bullet list is not a description.
func TestDescribeReadsProseAndFallsBackToTheHeading(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"prose wins over a heading", "# Deploy\n\nRun the release checklist before shipping.", "Run the release checklist before shipping."},
		{"heading is the fallback", "# Deploy\n\n", "Deploy"},
		{"a code fence is not a description", "# Deploy\n\n```sh\nnpm run build\n```\n\nThen ship it.", "Then ship it."},
		{"a bullet list is not a description", "# Deploy\n\n- step one\n- step two\n", "Deploy"},
		{"an html comment is skipped", "<!-- hidden -->\n\nThe actual description.", "The actual description."},
		{"wrapped prose collapses to one line", "# T\n\nfirst line\nsecond line\n\nthird", "first line second line"},
		{"empty is empty", "", ""},
		{"whitespace only is empty", "   \n\n\t\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Describe(tc.content); got != tc.want {
				t.Errorf("Describe(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

// A long skill still has to fit the catalogue as a line, on a rune boundary.
func TestDescribeIsBoundedAndStaysValidUTF8(t *testing.T) {
	got := Describe(strings.Repeat("é", 400))
	if len(got) > describeBudget+len("...") {
		t.Errorf("description is %d bytes, over the %d budget plus the ellipsis", len(got), describeBudget)
	}
	if !utf8.ValidString(got) {
		t.Errorf("description is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("a truncated description should say so: %q", got)
	}
}

// A skill with no prose and no heading is still listed. An empty description
// would be one more thing the model learns to ignore, and a missing skill is a
// skill it can never ask for.
func TestCatalogSaysSoWhenASkillHasNoDescription(t *testing.T) {
	catalog := RenderCatalog([]Skill{{Name: "bare", Versions: []Version{{Version: 1, Content: ""}}}}, 2000)
	if !strings.Contains(catalog, "No description") {
		t.Errorf("catalog hid a skill with nothing to say: %q", catalog)
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
