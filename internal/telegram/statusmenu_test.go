package telegram

import (
	"strings"
	"testing"

	"ceraa/internal/telegram/menus"
)

// The hub's Status button used to send the menu's own name as the entire message
// body, so it produced the word "status" above a row of buttons while /status
// printed a full report under an identical keyboard. Three paths, one menu, and the
// one people actually tap showed nothing.
func TestStatusMenuShowsAReportNotItsOwnName(t *testing.T) {
	isolateState(t)
	text, ok := menuReportText("status", 1)
	if !ok {
		t.Fatal("the status menu has no body text, so tapping it shows only a title")
	}
	if strings.TrimSpace(text) == "📋 status" {
		t.Fatal("the status menu body is just the menu name")
	}
	for _, want := range []string{"Session:", "Provider:", "Model:", "Messages:", "Cache read:"} {
		if !strings.Contains(text, want) {
			t.Errorf("the status report is missing %q:\n%s", want, text)
		}
	}
}

// /status and /context were two reports behind one keyboard, and picking either
// lost facts the other had: /status had agent, /context had tokens and
// cache. They are now one report.
func TestStatusAndContextAreTheSameReport(t *testing.T) {
	isolateState(t)
	status, okStatus := menuReportText("status", 1)
	context, okContext := menuReportText("context", 1)
	if !okStatus || !okContext {
		t.Fatal("one of the two entry points has no report")
	}
	if status != context {
		t.Errorf("status and context disagree:\n--- status ---\n%s\n--- context ---\n%s", status, context)
	}
}

// The merged report has to carry both halves, not just the richer one.
func TestStatusReportCarriesContextAndSessionFacts(t *testing.T) {
	isolateState(t)
	text, _ := menuReportText("status", 1)
	for _, want := range []string{"tokens", "Cache read:", "Session:", "Project:", "Agent:", "Endpoint:"} {
		if !strings.Contains(text, want) {
			t.Errorf("merged report lost %q:\n%s", want, text)
		}
	}
}

// A menu that is genuinely a list of buttons keeps its name as the label. Only the
// ones that report get a body, so this pins that the fix did not turn every menu
// into a wall of text.
func TestActionMenusKeepTheTitleFallback(t *testing.T) {
	isolateState(t)
	for _, name := range []string{"permission", "tools", "settings", "todos", "memory"} {
		if _, ok := menuReportText(name, 1); ok {
			t.Errorf("%q is an action list but was given a report body", name)
		}
	}
	// And a failing report still says so rather than silently becoming a title.
	broken, ok := menuReportText("status", 1)
	if !ok || strings.TrimSpace(broken) == "" {
		t.Error("a status report that cannot be built produced no body at all")
	}
}

// The keyboard under the report is unchanged: the buttons are shortcuts, not the
// content. Pinning it so a future change cannot quietly empty the screen again.
func TestStatusMenuStillOffersItsShortcuts(t *testing.T) {
	isolateState(t)
	callbacks := callbacksIn(t, menus.InlineForSession("status", 0, ""))
	if !hasCallback(callbacks, "compact:now") {
		t.Error("the status menu lost its Compact button")
	}
	if !hasCallback(callbacks, "menu:message_history") {
		t.Error("the status menu lost its Recent messages button")
	}
}
