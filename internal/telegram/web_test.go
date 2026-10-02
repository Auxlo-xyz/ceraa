package telegram

import (
	"errors"
	"strings"
	"testing"

	"ceraa/internal/miniapp"
)

// The command opens the surface instead of reporting that it is off. A refusal
// was a sentence about configuration when what was asked for is a link, and it
// handed the operator work the program can do itself.
func TestWebShareOpensTheSurfaceInsteadOfRefusing(t *testing.T) {
	isolateState(t)
	captured := beginCapture(t)
	opened := false
	start := func() (*miniapp.Server, error) {
		opened = true
		return nil, errors.New("session store unavailable: nothing to open")
	}
	handleWebShare("test-token", 1, start)

	if !opened {
		t.Fatal("the command never asked for the surface to be opened")
	}
	text := captured.text()
	if strings.Contains(text, "CERAA_MINIAPP=1") {
		t.Errorf("reply = %q, still tells the operator to set a flag and restart", text)
	}
	if !strings.Contains(text, "session store unavailable") {
		t.Errorf("reply = %q, want the reason it could not open", text)
	}
	if strings.Contains(text, "https://") {
		t.Errorf("reply = %q, want no address when nothing was opened", text)
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
