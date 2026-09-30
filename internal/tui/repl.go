package tui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// Loop runs the interactive session until the operator leaves.
//
// It is a line loop rather than a full-screen program on purpose. A readline
// prompt works over SSH, inside a pipe to less, and in a terminal of any size,
// and it keeps every line the run produced in the scrollback where it can be
// copied. A full-screen TUI is prettier and works in none of those cases.
type Loop struct {
	session *Session
	render  *Renderer
	in      io.Reader
	out     io.Writer
	pal     Palette
	version string
	// cancel is the current turn's cancel, held so Ctrl-C interrupts the run
	// rather than the loop. A second Ctrl-C is left to the default handler and
	// kills the process, which is the escape hatch for a wedged run.
	cancel      context.CancelFunc
	interrupted bool
}

// NewLoop builds a loop over a session.
func NewLoop(session *Session, in io.Reader, out io.Writer, pal Palette, version string) *Loop {
	return &Loop{session: session, render: session.render, in: in, out: out, pal: pal, version: version}
}

// Run drives the loop until EOF, /exit or an interrupt.
func (l *Loop) Run(ctx context.Context) error {
	provider, model := l.session.Model()
	fmt.Fprint(l.out, l.pal.Banner(l.version, model, provider))
	l.hint()
	sc := bufio.NewScanner(l.in)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	// Ctrl-C cancels the run rather than the loop, so a long turn can be stopped
	// without losing the conversation.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	interrupts := make(chan os.Signal, 1)
	go func() {
		for s := range sig {
			select {
			case interrupts <- s:
			default:
			}
		}
	}()
	for {
		l.prompt()
		if !sc.Scan() {
			// EOF: a piped prompt is the common non-interactive case, so the last
			// line read before the pipe closed is treated as the question rather
			// than being discarded.
			line := strings.TrimSpace(sc.Text())
			if line != "" {
				l.dispatch(ctx, line)
			}
			l.farewell()
			return nil
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		handled, done := l.slash(ctx, line)
		if done {
			l.farewell()
			return nil
		}
		if handled {
			continue
		}
		// A slash line the loop does not own is a typo, and saying so is better
		// than sending it to the model as a request.
		if strings.HasPrefix(line, "/") {
			l.render.line(l.pal.Dim("  unknown command " + line + " - try /help"))
			continue
		}
		l.dispatch(ctx, line)
		select {
		case <-interrupts:
		default:
		}
	}
}

// prompt writes the input prompt.
//
// The prompt carries the mascot's state so the operator can see whether the last
// turn finished without scrolling, and the session id so a transcript can be
// traced back to a conversation.
func (l *Loop) prompt() {
	st := l.render.State()
	mark := ">"
	if st == StateError {
		mark = "!"
	}
	prefix := l.pal.Bold("ceraa") + l.pal.Dim(" "+l.session.SessionID()+" ")
	fmt.Fprint(l.out, prefix+l.pal.Reverse(" "+mark+" ")+" ")
}

// hint prints the first-run key hints, once per process.
func (l *Loop) hint() {
	l.render.line(l.pal.Dim("  type a request, or /help for commands. Ctrl-C interrupts a run, Ctrl-D leaves."))
	l.render.line("")
}

// dispatch runs one turn, holding a cancel that Ctrl-C can fire.
func (l *Loop) dispatch(ctx context.Context, line string) {
	runCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	defer func() {
		cancel()
		l.cancel = nil
	}()
	if err := l.session.RunContext(runCtx, line); err != nil {
		// A cancelled run is the operator's own doing, so it is reported plainly
		// rather than as a failure.
		if isCanceled(err) {
			l.render.line(l.pal.Dim("  interrupted"))
			return
		}
		l.render.Error(err)
	}
}

func isCanceled(err error) bool {
	return err != nil && (err == context.Canceled || strings.Contains(err.Error(), "context canceled"))
}

// farewell prints the exit line.
func (l *Loop) farewell() {
	fmt.Fprint(l.out, "\n"+l.pal.Dim("  session "+l.session.SessionID()+" saved. bye.")+"\n")
}

// slash handles a command line and reports whether the loop should end.
//
// The commands are the small set that are useful without a second screen:
// status, help, sessions, a new conversation, doctor, and leaving. Anything that
// needs a form belongs in `ceraa config` or `ceraa doctor`, not in a prompt.
// slash handles a command line.
//
// It reports whether the line was handled and whether the loop should end, as two
// separate answers. They are separate because a handled command that does not end
// the loop and an unrecognised command both continue the loop, and collapsing them
// makes /help fall through to the "unknown command" branch and tell the user to
// try /help.
func (l *Loop) slash(ctx context.Context, line string) (handled, done bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false, false
	}
	cmd := strings.ToLower(fields[0])
	if !knownCommand(cmd) {
		return false, false
	}
	switch cmd {
	case "/exit", "/quit", "/q":
		return true, true
	}

	switch cmd {
	case "/help", "/?":
		l.help()
		return true, false

	case "/new":
		l.newSession()
		return true, false

	case "/status", "/s":
		l.status()
		return true, false

	case "/sessions":
		l.sessions()
		return true, false

	case "/model", "/provider":
		l.model()
		return true, false

	case "/clear":
		fmt.Fprint(l.out, "\r\x1b[2K")
		l.render.line(l.pal.Dim("  (scrollback is the terminal's; use /new for a fresh conversation)"))
		return true, false

	case "/doctor":
		l.render.line(l.pal.Dim("  run `ceraa doctor` for the full check"))
		return true, false

	case "/cost", "/usage":
		l.render.line(l.pal.Dim("  per-turn tokens and cost print in the footer after each run"))
		return true, false

	default:
		return false, false
	}
}

// knownCommand reports whether a slash line names a command the loop owns.
//
// Kept as one list so the dispatcher and the help output cannot drift: a command
// that exists but is not listed here would be reported as unknown while appearing
// in /help, which is the kind of small contradiction that costs an afternoon.
func knownCommand(cmd string) bool {
	switch cmd {
	case "/exit", "/quit", "/q", "/help", "/?", "/new", "/status", "/s",
		"/sessions", "/model", "/provider", "/clear", "/doctor", "/cost", "/usage":
		return true
	}
	return false
}

// help prints the command list.
func (l *Loop) help() {
	rows := [][2]string{
		{"/help", "this list"},
		{"/status", "active provider, model, session"},
		{"/sessions", "list saved conversations"},
		{"/new", "start a fresh conversation"},
		{"/model", "show the active model and provider"},
		{"/clear", "clear the screen"},
		{"/exit", "leave (Ctrl-D also works)"},
	}
	l.render.line("")
	l.render.line(l.pal.Bold("  commands"))
	for _, r := range rows {
		l.render.line("  " + l.pal.Bold(r[0]) + l.pal.Dim(strings.Repeat(" ", max(1, 10-len(r[0])))+r[1]))
	}
	l.render.line(l.pal.Dim("  anything else is sent to the model as a request"))
	l.render.line("")
}

// status prints what the next turn would run with.
func (l *Loop) status() {
	provider, model := l.session.Model()
	l.render.line("")
	l.render.line(l.pal.Bold("  status"))
	l.render.line("  " + l.pal.Dim("session  ") + l.session.SessionID())
	l.render.line("  " + l.pal.Dim("provider ") + provider)
	l.render.line("  " + l.pal.Dim("model    ") + model)
	l.render.line("  " + l.pal.Dim("workdir  ") + l.session.WorkDir())
	l.render.line("")
}

// model shows the active model and, when the config has more than one, the list.
func (l *Loop) model() {
	provider, model := l.session.Model()
	l.render.line("")
	l.render.line(l.pal.Bold("  model"))
	l.render.line("  " + l.pal.Dim("active   ") + model)
	l.render.line("  " + l.pal.Dim("provider ") + provider)
	var ids []string
	if l.session.cfg != nil {
		ids = l.session.cfg.ProviderIDs()
	}
	if len(ids) > 1 {
		l.render.line("  " + l.pal.Dim("available"))
		for _, id := range ids {
			mark := "  "
			if id == l.session.cfg.ActiveProvider {
				mark = "* "
			}
			p, _ := l.session.cfg.ProviderProfile(id)
			l.render.line("  " + mark + l.pal.Dim(pad(id, 14)) + p.Model)
		}
	}
	l.render.line(l.pal.Dim("  switch with `ceraa config` or the provider picker"))
	l.render.line("")
}

// sessions lists saved conversations.
func (l *Loop) sessions() {
	store := l.session.Store()
	if store == nil {
		l.render.line(l.pal.Dim("  no session store yet"))
		return
	}
	ids, err := store.List()
	if err != nil {
		l.render.line(l.pal.Dim("  could not read sessions: " + ShortErrorText(err.Error())))
		return
	}
	l.render.line("")
	l.render.line(l.pal.Bold("  sessions") + l.pal.Dim(fmt.Sprintf("  (%d)", len(ids))))
	// One current marker, not one per row. An earlier version appended "(current)"
	// to every row because the mark was computed from a slice index rather than
	// from the id, and a list where three of four entries claim to be current
	// answers none of the operator's questions.
	current := l.session.SessionID()
	for _, id := range ids {
		mark := "  "
		note := ""
		if id == current {
			mark = l.pal.Reverse("*")
			note = l.pal.Dim("  (current)")
		}
		l.render.line("  " + mark + " " + l.pal.Dim(pad(id, 26)) + note)
	}
	if len(ids) == 0 {
		l.render.line(l.pal.Dim("  none yet"))
	}
	l.render.line("")
}

// newSession starts a fresh conversation in the same process.
//
// The stores stay open and only the runner is rebuilt, so /new is instant and
// keeps the configured provider rather than re-reading config from disk.
func (l *Loop) newSession() {
	if l.session == nil {
		l.render.line(l.pal.Dim("  no session to replace"))
		return
	}
	l.session.session = newSessionID()
	if err := l.session.build(); err != nil {
		l.render.Error(err)
		return
	}
	l.render.line(l.pal.Dim("  new session " + l.session.session))
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
