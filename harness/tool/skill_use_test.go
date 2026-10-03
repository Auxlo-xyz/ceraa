package tool_test

import (
	"context"
	"strings"
	"testing"

	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/skills"
	"ceraa/harness/tool"
)

// skillUse builds the tool over a real store, because the seam being tested is
// the store read and a stub would agree with whatever the test assumed.
func skillUse(t *testing.T) (*tool.SkillUseTool, *skills.Store) {
	t.Helper()
	store, err := skills.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &tool.SkillUseTool{Store: store}, store
}

// The two halves have to agree: a name in the catalogue is a name that
// resolves. If they ever drift the model is told a skill exists and then told
// it does not, which is worse than not listing it.
func TestTheCatalogueAndTheToolResolveTheSameNames(t *testing.T) {
	tool, store := skillUse(t)
	if _, err := store.AddVersion("write-migrations", "# Write migrations\n\nUse when changing the schema."); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.CatalogContext(2000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(catalog, `name="write-migrations"`) {
		t.Fatalf("catalogue does not list the skill: %q", catalog)
	}
	spec, err := tool.Translate(map[string]any{"name": "write-migrations"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("a skill the catalogue advertises did not resolve: %v", err)
	}
	if !strings.Contains(body, "Use when changing the schema.") {
		t.Errorf("body did not carry the skill: %q", body)
	}
	if !strings.Contains(body, "version 1") {
		t.Errorf("body did not say which version it is: %q", body)
	}
}

// The body is the point. It must arrive in full even when the catalogue
// refused to carry it.
func TestTheBodyArrivesEvenWhenTheCatalogueIsTooSmallToShowIt(t *testing.T) {
	tool, store := skillUse(t)
	body := strings.Repeat("an instruction the model needs. ", 200)
	if _, err := store.AddVersion("long", "# Long\n\n"+body); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.CatalogContext(120)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) > 120 {
		t.Fatalf("catalogue is over budget: %d", len(catalog))
	}
	spec, err := tool.Translate(map[string]any{"name": "long"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "an instruction the model needs.") {
		t.Error("a large skill did not survive being kept out of the catalogue")
	}
}

// A miss is answered with the names that do resolve. The store's own error is
// accurate and useless to a model that guessed.
func TestAMissListsTheNamesThatDoResolve(t *testing.T) {
	tool, store := skillUse(t)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := store.AddVersion(name, "# "+name+"\n\nDoes the thing."); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := tool.Translate(map[string]any{"name": "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.RunContext(context.Background(), spec)
	if err == nil {
		t.Fatal("an unknown skill resolved")
	}
	for _, want := range []string{"gamma", "alpha, beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// An unapproved proposal is not reachable. That is what makes a catalogue of
// names safe to shrink to: the approval gate is the store, not the prompt.
func TestAnUnapprovedProposalIsNotReachable(t *testing.T) {
	tool, store := skillUse(t)
	proposal, err := store.Propose("pending-skill", "# P\n\nNot approved yet.", "needs review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Propose("second", "second pending", "also needs review"); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.CatalogContext(2000)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(catalog, "pending-skill") {
		t.Fatalf("an unapproved skill is in the catalogue: %q", catalog)
	}
	spec, err := tool.Translate(map[string]any{"name": "pending-skill"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.RunContext(context.Background(), spec); err == nil {
		t.Error("an unapproved skill resolved")
	}
	_ = proposal
}

// The name is required. A nameless call cannot be answered and would otherwise
// read the whole store.
func TestTheNameIsRequired(t *testing.T) {
	tool, _ := skillUse(t)
	for _, args := range []map[string]any{nil, {}, {"name": ""}, {"name": "   "}, {"name": 7}} {
		if _, err := tool.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted a call with no usable name", args)
		}
	}
}

// The spec is persisted, so the name it carries is the validated one rather than
// whatever spacing arrived with it.
func TestThePersistedSpecCarriesTheNormalisedName(t *testing.T) {
	tool, _ := skillUse(t)
	spec, err := tool.Translate(map[string]any{"name": "  spaced-name  "})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := spec.Args["name"].(string); got != "spaced-name" {
		t.Errorf("spec name = %q, want the trimmed name", got)
	}
	if spec.Type != "skill_use" {
		t.Errorf("spec type = %q, want skill_use so the dispatcher resolves the tool by it", spec.Type)
	}
}

// A tool with no store cannot pretend. Saying so is better than an empty body
// the model reads as an instruction to follow nothing.
func TestNoStoreIsRefusedRatherThanFaked(t *testing.T) {
	bare := &tool.SkillUseTool{}
	spec, err := bare.Translate(map[string]any{"name": "anything"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.RunContext(context.Background(), spec); err == nil {
		t.Fatal("a tool with no store returned a result")
	}
}

// Cancelled before it starts is a cancelled call, not a skill read.
func TestCancellationIsHonoured(t *testing.T) {
	tool, store := skillUse(t)
	if _, err := store.AddVersion("alpha", "# A\n\nDoes the thing."); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	spec, err := tool.Translate(map[string]any{"name": "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.RunContext(ctx, spec); err == nil {
		t.Fatal("a cancelled call returned a body")
	}
}

// The result path is the identity, so a stored body reaches the transcript
// exactly as the tool produced it.
func TestTranslateResultIsTheOutput(t *testing.T) {
	tool, store := skillUse(t)
	if _, err := store.AddVersion("alpha", "# A\n\nDoes the thing."); err != nil {
		t.Fatal(err)
	}
	spec, err := tool.Translate(map[string]any{"name": "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{Status: operation.StatusCompleted, Output: out}
	if got := tool.TranslateResult(op); got != out {
		t.Errorf("TranslateResult changed the output")
	}
}

// The tool is registered only when there is a store to serve it from.
func TestTheToolIsRegisteredOnlyWithAStore(t *testing.T) {
	if _, ok := tool.DefaultRegistryForUser("x", nil, nil).Get("skill_use"); ok {
		t.Error("skill_use is offered with no store behind it")
	}
	store, err := skills.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tool.DefaultRegistryForUser("x", nil, store).Get("skill_use"); !ok {
		t.Error("skill_use is missing from a registry that has a store")
	}
}

// The gate is on proposing, not on reading. The catalogue is rendered into the
// prompt whatever the user said, so a reader gated on the same words would leave
// the model told a skill exists and unable to ask for it.
func TestTheGateWithdrawsTheProposerAndNotTheReader(t *testing.T) {
	store, err := skills.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A real memory store, because the branch that changed is the memory one and
	// a nil store would leave the memory tool absent for reasons that have
	// nothing to do with the gate.
	memoryStore, err := memory.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		user            string
		wantPropose     bool
		wantSkillUse    bool
		wantMemoryOffer bool
	}{
		{"cli", false, true, false},
		{"that is wrong", true, true, false},
		{"remember this", false, true, true},
		{"remember this, it is wrong", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.user, func(t *testing.T) {
			registry := tool.DefaultRegistryForUser(tc.user, memoryStore, store)
			if _, ok := registry.Get(tool.SkillProposalName); ok != tc.wantPropose {
				t.Errorf("%s: skill_propose offered = %v, want %v", tc.user, ok, tc.wantPropose)
			}
			if _, ok := registry.Get("skill_use"); ok != tc.wantSkillUse {
				t.Errorf("%s: skill_use offered = %v, want %v", tc.user, ok, tc.wantSkillUse)
			}
			if _, ok := registry.Get("memory"); ok != tc.wantMemoryOffer {
				t.Errorf("%s: memory offered = %v, want %v", tc.user, ok, tc.wantMemoryOffer)
			}
		})
	}
}
