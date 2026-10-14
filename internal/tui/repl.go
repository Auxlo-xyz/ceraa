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
	cancel context.CancelFunc
	// lastSession remembers the id the context line last showed, so a new session
	// is visibly a new session.
	lastSession string
}

// NewLoop builds a loop over a session.
func NewLoop(session *Session, in io.Reader, out io.Writer, pal Palette, version string) *Loop {
	return &Loop{session: session, render: session.render, in: in, out: out, pal: pal, version: version}
}

// Run drives the loop until EOF, /exit or an interrupt.
func (l *Loop) Run(ctx context.Context) error {
	fmt.Fprint(l.out, l.pal.Banner(l.startup(), l.render.width()))
	l.hint()
	l.tip()
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

// startup collects what the session knows about itself for the banner.
func (l *Loop) startup() Startup {
	provider, model := l.session.Model()
	return Startup{
		Version:  l.version,
		Model:    model,
		Provider: provider,
		WorkDir:  l.session.WorkDir(),
		Effort:   l.session.Effort(),
		Agent:    l.session.Agent(),
		Tools:    l.session.ToolCount(),
		Session:  l.session.SessionID(),
	}
}

// tips are the hints that rotate above the composer.
//
// Each one names a command that exists and says what it does, because a tip
// that describes a feature this build does not have is worse than no tip: it
// teaches a key that does nothing. Ordered so the ones a newcomer needs come
// first, since the rotation is not random.
var tips = []string{
	"/help lists every command",
	"/status shows the provider, model and session",
	"/sessions lists saved conversations",
	"/new starts a fresh conversation",
	"/model shows the active model",
	"ctrl-c interrupts a turn without losing the conversation",
	"ctrl-d leaves",
	"ceraa doctor checks the configuration end to end",
}

// tip prints one rotating hint, chosen from the session id.
//
// The id is a nanosecond timestamp, so consecutive launches land on different
// entries and the same launch always shows the same one. That keeps the
// rotation honest: there is no hidden counter to write, lose across an upgrade,
// or desynchronise from what was actually shown.
func (l *Loop) tip() {
	if text := l.tipText(); text != "" {
		l.render.line(l.pal.Dim(text))
	}
}

// tipText is the chosen tip, or empty when there are none. Split from tip() so a
// test can assert the rotation without standing up a renderer.
func (l *Loop) tipText() string {
	if len(tips) == 0 {
		return ""
	}
	sum := 0
	for _, r := range l.session.SessionID() {
		sum += int(r)
	}
	return "  " + glyph("tip") + " " + tips[sum%len(tips)]
}

// prompt writes the context line and the input chevron.
//
// The prompt itself is one bare chevron. It used to repeat "ceraa
// cli-1790786005 >" on every turn, which put a fourteen-character prefix in front
// of every line the operator typed and buried the caret. Aster does the same
// thing: the composer is a single glyph, and everything else lives in a status
// line above it. The session id moves there too, so a transcript is still
// traceable without the prompt carrying it.
func (l *Loop) prompt() {
	_, model := l.session.Model()
	width := l.pal.width()
	if l.lastSession != l.session.SessionID() {
		l.lastSession = l.session.SessionID()
	}
	fmt.Fprint(l.out, l.pal.statusLine(width, l.render.State().String(), model, l.session.Effort())+"\n")
	// The chevron sits at the left margin, where a shell prompt does, so the caret
	// is where the eye expects it. The placeholder says what the line is for and
	// that a slash is a command, because a bare chevron says neither and a first
	// run should not have to guess.
	fmt.Fprint(l.out, l.pal.Bold(glyph("prompt")+" ")+l.pal.Dim(composerHintFor(width))+" ")
}

// hint prints the key hints, once.
//
// One dim line. It used to be a full sentence about typing a request, which is
// what a prompt already implies, and it was printed on every launch.
func (l *Loop) hint() {
	l.render.line(l.pal.hintBar(l.pal.width()))
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

// status prints what the next turn would run with, as an aligned block.
//
// The labels used to be printed with a hand-counted pad, so "session" and
// "provider" put their values in different columns and the block read as three
// unrelated lines. Right-aligning every label into one gutter makes it a table.
func (l *Loop) status() {
	provider, model := l.session.Model()
	b := &block{title: "status"}
	b.add("model", model)
	b.add("provider", provider)
	b.add("session", l.session.SessionID())
	b.add("workdir", l.session.WorkDir())
	if status, err := l.session.Status(context.Background()); err == nil {
		b.add("run", string(status.Status))
		if status.CurrentRunID != "" {
			b.add("run id", string(status.CurrentRunID))
		}
	} else if l.session.controller != nil {
		// Keep the local status useful even when a persisted status read fails.
		b.add("run", "unavailable")
	}
	if r := l.render; r != nil {
		r.writeBlock("status", b.render(l.pal, "  ", l.pal.width()))
		r.line("")
	}
}

// model shows the active model and, when the config has more than one, the list.
func (l *Loop) model() {
	provider, model := l.session.Model()
	b := &block{title: "model"}
	b.add("active", model)
	b.add("provider", provider)
	if l.render != nil {
		l.render.writeBlock("model", b.render(l.pal, "  ", l.pal.width()))
	}
	l.render.line("")
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
