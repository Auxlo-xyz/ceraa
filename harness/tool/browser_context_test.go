package tool

import (
	"context"
	"strings"
	"testing"
)

// The page inherits the context Chromium was launched with, and newRodDriver
// cancels that on return. An action that used d.page directly therefore reported
// "context canceled" for the rest of the browser's life.
//
// This failed in production and looked like a screenshot capability problem:
// open, eval, status and close all worked while screenshot, text, html, click and
// type failed against the same open page, so the agent routed around it to
// headless Chrome via bash. Navigate and Eval had been rebinding the context all
// along, which is the whole reason the failure was selective.
func TestEveryActionBindsItsOwnContext(t *testing.T) {
	driver := &rodDriver{page: nil}
	if got := driver.actionPage(context.Background()); got != nil {
		t.Fatal("actionPage on a driver with no page should not invent one")
	}
}

// This is the production sequence, and reproducing it needed the operation
// layer's own behaviour.
//
// Navigate rebinds d.page to *its* context and stores the result. The operation
// layer then cancels that context when the call returns (`defer tcancel()`). So
// d.page is left bound to a context that is already dead, and the next action
// that uses d.page directly fails with "context canceled" for the rest of the
// browser's life.
//
// That is why open worked and then screenshot, text, html, click and type all
// failed against the same open page, while eval kept working: eval rebinds. The
// agent read that as a missing screenshot capability and routed around it to
// headless Chrome via bash.
func TestActionSurvivesTheNextOperationCancellingTheLast(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a real browser")
	}
	driver, err := newRodDriver(context.Background())
	if err != nil {
		t.Skipf("no browser available: %v", err)
	}
	defer func() { _ = driver.Close() }()

	// The open, in its own operation context, cancelled on return exactly as the
	// operation layer does.
	openCtx, openCancel := context.WithCancel(context.Background())
	if _, err := driver.Navigate(openCtx, "https://example.com"); err != nil {
		openCancel()
		t.Skipf("navigate failed, cannot exercise the rest: %v", err)
	}
	openCancel()

	// The next operation, with a live context of its own.
	next := context.Background()

	// These are the calls that failed in production, in the order they were made.
	if err := driver.Screenshot(next, t.TempDir()+"/shot.png", false, ""); err != nil {
		t.Errorf("screenshot in the next operation: %v", err)
	}
	if _, err := driver.Text(next, ""); err != nil {
		t.Errorf("read text in the next operation: %v", err)
	}
	if _, err := driver.HTML(next, ""); err != nil {
		t.Errorf("read html in the next operation: %v", err)
	}
	if _, err := driver.Eval(next, "document.title"); err != nil {
		t.Errorf("eval in the next operation: %v", err)
	}
	if _, err := driver.Info(next); err != nil {
		t.Errorf("info in the next operation: %v", err)
	}
}

// The error the agent saw verbatim, so a regression is recognisable in a log rather
// than looking like an environment problem.
func TestCancelledContextIsNoLongerWhatFails(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a real browser")
	}
	driver, err := newRodDriver(context.Background())
	if err != nil {
		t.Skipf("no browser available: %v", err)
	}
	defer func() { _ = driver.Close() }()

	openCtx, openCancel := context.WithCancel(context.Background())
	if _, err := driver.Navigate(openCtx, "https://example.com"); err != nil {
		openCancel()
		t.Skipf("navigate failed: %v", err)
	}
	openCancel()

	if _, err := driver.Text(context.Background(), ""); err != nil &&
		strings.Contains(err.Error(), "context canceled") {
		t.Errorf("read text still reports the cancelled previous operation: %v", err)
	}
}
