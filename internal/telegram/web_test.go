package telegram

import (
	"strings"
	"testing"
)

// The surface is off for anyone who has not asked for it, so this is the first
// branch of the command most people will ever hit. The reply has to name the
// setting that turns it on: answering with no address and no instruction
// leaves the reader with a command that did nothing they can act on.
func TestWebShareSaysHowToTurnTheSurfaceOn(t *testing.T) {
	isolateState(t)
	captured := beginCapture(t)
	handleWebShare("test-token", 1, nil)

	text := captured.text()
	if !strings.Contains(text, "CERAA_MINIAPP=1") {
		t.Errorf("reply = %q, want it to name the setting that turns the surface on", text)
	}
	if strings.Contains(text, "https://") {
		t.Errorf("reply = %q, want no address when there is no surface", text)
	}
}

// The command has to be in the catalog as well as in the dispatcher, because
// the catalog is what puts it in Telegram's own command menu and in /help. A
// case in the switch with no catalog entry is a command nobody discovers.
func TestWebShareIsAdvertisedAsACommand(t *testing.T) {
	for _, command := range commandCatalog() {
		if command.Command != "web" {
			continue
		}
		if command.Description == "" {
			t.Error("/web is advertised with no description")
		}
		return
	}
	t.Error("/web is not in the command catalog, so /help will not mention it")
}
