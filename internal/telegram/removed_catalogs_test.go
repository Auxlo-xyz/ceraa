package telegram

import (
	"strings"
	"testing"

	"ceraa/internal/telegram/menus"
)

// The evaluation screen, the command catalog and the MCP catalog were withdrawn
// from the hub. Removing a hub entry does not remove a feature, and each of these
// had more than one way in: a resolver case, an advertised slash command, a
// run_command callback route, and for eval an eval:view button of its own.
//
// These tests drive the entry points rather than inspecting the source, because a
// dead code path and a removed code path look identical to a grep. What has to
// hold is the behaviour: an old message still sitting in a chat must do nothing,
// and it must not claim to have done something.
var removedCatalogCommands = []string{"/eval", "/command_catalog", "/mcp_catalog"}

// A stale eval:view tap must be inert. It used to re-send the whole baseline
// report, so the failure mode to guard against is a reply that looks like a
// working feature rather than nothing at all.
func TestStaleEvalCallbackIsInert(t *testing.T) {
	isolateState(t)
	cap := beginCapture(t)
	tap(t, "eval:view")

	text := cap.text()
	for _, banned := range []string{"Evaluation baseline", "Success rate", "Cost per known-cost task"} {
		if strings.Contains(text, banned) {
			t.Errorf("a stale eval:view tap still produced a report (%q present): %q", banned, text)
		}
	}
}

// A hub keyboard sent before the removal carries menu:eval and the other two.
// They resolve to a bare Close, which edits nothing and claims nothing.
func TestStaleRemovedCatalogMenuCallbacksRenderNoMenu(t *testing.T) {
	isolateState(t)
	for _, name := range []string{"eval", "command_catalog", "mcp_catalog"} {
		cap := beginCapture(t)
		tap(t, "menu:"+name)

		if strings.Contains(cap.text(), "📈") || strings.Contains(cap.text(), "⌘") {
			t.Errorf("menu:%s still rendered a catalog screen: %q", name, cap.text())
		}
	}
}

// The typed command must be gone too, not just the button. /eval and
// /mcp_catalog and /command_catalog each had a dispatch branch; leaving one would
// keep the feature reachable by typing, which is the case a hub change alone does
// not cover.
//
// This asserts on the payload Telegram actually receives rather than on
// menus.Commands(), because the thing that puts a command in a user's client
// command menu is the setMyCommands body, not the slice it came from. A stale
// entry in that slice is a button in every chat that has the bot open.
func TestRemovedCatalogsAreNotSentToTelegramAsCommands(t *testing.T) {
	raw, err := setMyCommandsBody()
	if err != nil {
		t.Fatalf("setMyCommands body: %v", err)
	}
	body := string(raw)
	for _, name := range []string{"eval", "command_catalog", "mcp_catalog"} {
		if strings.Contains(body, `"/`+name+`"`) || strings.Contains(body, `"/`+strings.ReplaceAll(name, "_", "-")+`"`) {
			t.Errorf("%s is still published to Telegram as a command: %s", name, body)
		}
	}
}

// The shared catalog dispatcher is the other half of that. It survives, because
// /skills_catalog still uses it, so its cases for the two removed catalogs had
// to go individually rather than by deleting the function. A case left behind
// would keep a typed /mcp_catalog working with no button anywhere pointing at it,
// which is the hardest kind of leftover to notice.
//
// /eval needs no equivalent test: handleEvalCommand was deleted outright, so the
// compiler already proves nothing calls it. That is a stronger guarantee than any
// assertion written here would be.
func TestRemovedCatalogsNoLongerDispatch(t *testing.T) {
	isolateState(t)
	for _, command := range removedCatalogCommands {
		cap := beginCapture(t)
		handleCatalogCommand("test-token", 1, command)
		text := cap.text()
		if !strings.Contains(text, "Unknown catalog") {
			t.Errorf("%s still dispatched: %q", command, text)
		}
		if strings.Contains(text, "MCP catalog") || strings.Contains(text, "Show bot commands") {
			t.Errorf("%s produced its own catalog response: %q", command, text)
		}
	}
}

// /skills_catalog shares that dispatcher and must keep working, so removing the
// other two cases is not a reason to break it.
func TestSkillsCatalogStillDispatches(t *testing.T) {
	isolateState(t)
	cap := beginCapture(t)
	handleCatalogCommand("test-token", 1, "/skills_catalog")
	if strings.Contains(cap.text(), "Unknown catalog") {
		t.Errorf("removing the sibling cases broke /skills_catalog: %q", cap.text())
	}
}

// The whole set, asserted from one place: none of the three names may appear in
// the hub, in the advertised commands, or in anything the menus render. This is
// the guard that fails if a future change re-adds one by accident, which is the
// way these come back: a menu entry looks harmless on its own.
func TestNoRemovedCatalogSurfacesAnywhere(t *testing.T) {
	isolateState(t)
	for _, name := range []string{"eval", "command_catalog", "mcp_catalog"} {
		for _, offered := range menus.All {
			if offered == name {
				t.Errorf("%s is offered by the hub again", name)
			}
		}
		for _, command := range menus.Commands() {
			if command.Command == name {
				t.Errorf("%s is advertised to Telegram again", name)
			}
		}
	}
}
