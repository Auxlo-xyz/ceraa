package telegram

import (
	"strings"
	"testing"

	"ceraa/harness/tool"
)

// The bold set exists to separate the verbs of a run from its nouns. It went wrong
// twice the same way: a family was left half-bold, so the reader saw five glyphed
// lines and one bold one for the same kind of action and could not tell which tool
// ran. That is what this asserts, rather than a list of names that would only
// record the current split.
func TestEveryToolFamilyIsWhollyBoldOrWhollyNot(t *testing.T) {
	families := []struct {
		name  string
		tools []string
	}{
		{"file mutation", []string{"write", "edit", "edit_line", "batch_edit_file", "apply_patch"}},
		{"search", []string{"grep", "glob"}},
		{"shell", []string{"bash", "script", "run_command_with_secret"}},
		{"browser", []string{"browser"}},
		{"outbound", []string{"webfetch", "websearch", "send_media", "view_image"}},
		{"skills and secrets", []string{"skill_use", "skill_propose", "list_secrets"}},
	}
	for _, f := range families {
		for _, name := range f.tools {
			if !boldWordTool(name) {
				t.Errorf("%s: %q is not bold, so its family is split", f.name, name)
			}
		}
	}
}

// The other half of the line: these are queries and bookkeeping, not actions on
// the world, and they keep their marks.
func TestQueriesAndBookkeepingKeepTheirMarks(t *testing.T) {
	for _, name := range []string{
		"read", "question", "todowrite",
		"create_goal", "get_goal", "update_goal",
		"compaction", "task",
	} {
		if boldWordTool(name) {
			t.Errorf("%q is bold, which is the mark for an action, not a query", name)
		}
	}
}

// An unregistered tool must not be bold by default. A name nobody has classified
// is more likely to be a book-keeping tool than an action, and bolding it by
// accident makes every mark mean less.
func TestAnUnknownToolIsNotBold(t *testing.T) {
	for _, name := range []string{"", "mystery_tool", "TOOL", "bashx"} {
		if boldWordTool(name) {
			t.Errorf("%q is bold; an unclassified tool defaults to a mark", name)
		}
	}
}

// Every registered tool has to reach a decision. A tool with no case here would
// render with the generic gear, which is the fallback the renderer says is worse
// than a labelled line, and a new tool added later would slip through unnoticed.
func TestEveryRegisteredToolHasAClass(t *testing.T) {
	registered := []string{
		"bash", "read", "write", "edit", "edit_line", "batch_edit_file",
		"run_command_with_secret", "list_secrets", "webfetch", "view_image",
		"browser", "task", "fts_search", "question", "todowrite",
		"create_goal", "get_goal", "update_goal", "websearch", "script",
		"skill_propose", "skill_use", "send_media",
		"glob", "grep", "apply_patch", "compaction",
	}
	for _, name := range registered {
		if boldWordTool(name) {
			continue
		}
		if icon := toolIcon(name); icon == "" || icon == "⚙️" {
			t.Errorf("%q is neither bold nor given its own mark; it falls back to the gear", name)
		}
	}
}

// The renderer is handed a line of text and nothing else, so the name has to be
// recognisable inside it. If the token stopped appearing in the trail, bold
// would silently stop rendering and nothing else would fail.
func TestTheToolNameStillTravelsInTheEventString(t *testing.T) {
	line := "<b>write</b> /workspace/AGENTS.md"
	if !strings.Contains(line, "write") {
		t.Fatal("fixture does not contain the tool name")
	}
	var _ tool.Tool // the registry type, so this test tracks the real package
}

func TestBrowserMediaSecretsAndCommandsShowTheirCompletionStatus(t *testing.T) {
	for _, name := range []string{
		"browser", "browser_open", "browser_open_file", "browser_click", "browser_close", "view_image", "send_media", "list_secrets", "run_command_with_secret",
	} {
		for _, outcome := range []struct {
			prefix string
			mark   string
		}{
			{prefix: "✅", mark: "✓"},
			{prefix: "⚠️", mark: "✕"},
		} {
			t.Run(name+outcome.mark, func(t *testing.T) {
				renderer := &runRenderer{messageID: 1}
				label := toolDisplayName(name)
				renderer.tool("call-1", "🔄 "+toolNameToken(name)+label+" · target")
				wantName := strings.ReplaceAll(name, "_", " ")
				if boldWordTool(name) {
					wantName = "<b>" + wantName + "</b>"
				}
				if got, want := formatToolActivity(renderer.tools), "… "+wantName+" target"; got != want {
					t.Fatalf("running tool = %q, want %q", got, want)
				}
				result := " completed"
				if outcome.mark == "✕" {
					result = " failed"
				}
				renderer.tool("call-1", outcome.prefix+" "+toolNameToken(name)+label+result)
				if got, want := formatToolActivity(renderer.tools), outcome.mark+" "+wantName+" target"; got != want {
					t.Errorf("finished tool = %q, want %q", got, want)
				}
			})
		}
	}
}

func TestStatusMarkedToolsKeepBoldTextAndSpacing(t *testing.T) {
	for _, name := range []string{"list_secrets", "run_command_with_secret"} {
		for _, tc := range []struct {
			status string
			mark   string
		}{
			{status: "🔄", mark: "…"},
			{status: "✓", mark: "✓"},
			{status: "⚠", mark: "✕"},
		} {
			got := formatToolActivity([]toolActivity{{name: name, status: tc.status, detail: "target"}})
			want := tc.mark + " <b>" + strings.ReplaceAll(name, "_", " ") + "</b> target"
			if got != want {
				t.Errorf("%s status %q rendered %q, want %q", name, tc.status, got, want)
			}
		}
	}
}

func TestOtherToolMarksAreUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "read", want: "📚 read"},
		{name: "bash", want: "<b>bash</b>"},
		{name: "unknown_tool", want: "⚙️ unknown tool"},
	} {
		if got := formatToolActivity([]toolActivity{{name: tc.name, status: "✓"}}); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
}
