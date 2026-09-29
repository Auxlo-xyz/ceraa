package menus

import (
	"encoding/json"
	"strings"
	"testing"
)

// The evaluation screen, the command catalog and the MCP catalog were removed
// rather than hidden. None was wanted: the evaluation dashboard was never opened,
// the command catalog restated /help as a grid of buttons, and the MCP catalog
// had no servers to list, so it only ever rendered its own empty state.
//
// Removing an entry is not the same as removing the feature. Each of these had a
// menu builder, a resolver case in both InlineForChat and InlineForSession, an
// entry in the advertised command list that setMyCommands sends to Telegram, a
// label in the hub grid, a slash-command dispatch, a run_command callback route,
// and for eval a tap handler of its own. Any one of those left behind is a way
// back into something that is supposed to be gone.
var removedCatalogs = []string{"eval", "command_catalog", "mcp_catalog"}

func TestRemovedCatalogsAreGoneFromTheHub(t *testing.T) {
	for _, name := range removedCatalogs {
		for _, offered := range All {
			if offered == name {
				t.Errorf("the hub still offers %q", name)
			}
		}
	}
}

// The grid is built from All, so an All that is clean cannot carry them. The
// label map is a separate literal and did: leaving "eval" in it after removing
// eval from All is a dead entry that reads as live configuration to anyone
// reading the function.
func TestRemovedCatalogsHaveNoHubLabel(t *testing.T) {
	raw, err := json.Marshal(Hub())
	if err != nil {
		t.Fatalf("hub: %v", err)
	}
	markup := string(raw)
	for _, name := range removedCatalogs {
		if strings.Contains(markup, "menu:"+name) {
			t.Errorf("the hub still emits menu:%s: %s", name, markup)
		}
	}
	for _, label := range []string{"📈 Eval", "⌘ Commands", "🔌 MCP"} {
		if strings.Contains(markup, label) {
			t.Errorf("the hub still carries the label %q: %s", label, markup)
		}
	}
}

// Commands() is what setMyCommands publishes and what /help prints, so an entry
// left here advertises a command that no longer dispatches. Telegram would list
// it in the client's command menu and the user would get silence or an unknown
// command in reply.
func TestRemovedCatalogsAreNotAdvertisedAsCommands(t *testing.T) {
	for _, command := range Commands() {
		for _, name := range removedCatalogs {
			if command.Command == name {
				t.Errorf("%q is still advertised to Telegram with description %q", name, command.Description)
			}
		}
	}
}

// A hub keyboard sent before this change still carries menu:eval and friends.
// Those names resolve through the default to a bare Close, which is inert: no
// state changes and nothing claims to have worked. It is not an error path,
// because a menu that cannot be opened is the correct outcome for one that was
// withdrawn.
//
// This also pins the resolution rather than assuming it. If someone adds a case
// for one of these names back, the test fails here instead of the feature
// quietly returning.
func TestRemovedCatalogCallbacksResolveToInertClose(t *testing.T) {
	for _, name := range removedCatalogs {
		for _, markup := range []map[string]any{
			InlineForChat(name, 0, 1, ""),
			InlineForSession(name, 0, ""),
		} {
			rows, ok := markup["inline_keyboard"].([][]map[string]string)
			if !ok {
				t.Errorf("%s produced no keyboard: %#v", name, markup)
				continue
			}
			only := 0
			total := 0
			for _, row := range rows {
				for _, button := range row {
					total++
					if button["callback_data"] == "close" {
						only++
					}
				}
			}
			if total != only {
				t.Errorf("%s still renders %d button(s) that are not Close: %#v", name, total-only, markup)
			}
		}
	}
}

// The builders themselves are gone rather than left for something else to reach.
// evalMenu and commandMenu had no other caller, and commandMenu was the only
// reason a removed command would have kept a button pointing at it: it builds a
// run_command button for every entry in Commands(), so leaving it would have
// re-routed any stale catalog menu.
func TestRemovedCatalogBuildersAreGone(t *testing.T) {
	// A named menu that is no longer in All must not still resolve to something
	// with content, which is the same assertion as above but stated from the
	// builder side: there is no menu left for these names to open.
	for _, name := range removedCatalogs {
		if !isBareClose(Inline(name, 0)) {
			t.Errorf("%s still has a menu builder behind it: %#v", name, Inline(name, 0))
		}
	}
}
