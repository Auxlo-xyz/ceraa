package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"ceraa/internal/build"
	"ceraa/internal/config"
	"ceraa/internal/tui"
	"github.com/spf13/cobra"
)

// chatCmd is the interactive terminal session.
//
// It is a separate command rather than the bare root so that `ceraa "do the
// thing"` keeps its one-shot meaning and its scriptability, and so an
// interactive run is something the operator asks for rather than something that
// happens because no argument was typed.
var chatCmd = &cobra.Command{
	Use:   "chat",
	Short: "Talk to Ceraa in the terminal",
	Long: `Open an interactive session.

Each line is a request. The answer streams in as the model writes it, tool calls
are shown as they start and finish, and the turn ends with its token and cost
summary. Context carries across lines, so this is a conversation and not a series
of unrelated prompts.

Commands:
  /help              list commands
  /status            active provider, model and session
  /sessions          list saved conversations
  /new               start a fresh conversation
  /model             show the active model
  /clear             clear the screen
  /exit              leave

Ctrl-C interrupts a running turn without losing the conversation, and Ctrl-D
leaves. Writes and commands that touch the network or delete something ask
first, and "a" allows one for the rest of the session.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runChat("")
	},
}

var (
	chatSessionFlag string
	chatNoColorFlag bool
)

func init() {
	chatCmd.Flags().StringVar(&chatSessionFlag, "session", "", "continue a saved conversation by id")
	chatCmd.Flags().BoolVar(&chatNoColorFlag, "no-color", false, "disable colour output")
}

// runChat opens an interactive session.
//
// The config is loaded here rather than in the tui package so a configuration
// error is reported by the command that owns configuration, and so `ceraa chat`
// and `ceraa doctor` fail identically.
func runChat(sessionID string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if sessionID == "" {
		sessionID = chatSessionFlag
	}
	pal := tui.NewPalette(!chatNoColorFlag)
	// The renderer writes to stderr and the prompts read stdin, so `ceraa chat`
	// still works when stdout is redirected, and a transcript of the model is
	// still capturable on its own.
	renderer := tui.NewRenderer(os.Stderr, pal)
	session, err := tui.NewSession(tui.SessionOptions{
		Config:    cfg,
		Renderer:  renderer,
		SessionID: sessionID,
	})
	if err != nil {
		return err
	}
	loop := tui.NewLoop(session, os.Stdin, os.Stderr, pal, build.Version)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return loop.Run(ctx)
}

// chatOnce answers a single prompt and exits, keeping stdout clean.
//
// This is the shape a script wants, and it is why the loop is not the only
// entry: piping a question in and capturing the answer is a different shape from
// holding a conversation, and a tool that only offers one of them forces a
// workaround for the other.
var chatOnceCmd = &cobra.Command{
	Use:   "ask [prompt]",
	Short: "Answer one prompt and exit",
	Args:  cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		prompt := strings.Join(args, " ")
		if prompt == "" {
			// A piped prompt is how this gets used in a script, and reading stdin
			// is what makes `git diff | ceraa ask` work.
			data, err := readAllStdin()
			if err != nil {
				return err
			}
			prompt = strings.TrimSpace(data)
		}
		if prompt == "" {
			return fmt.Errorf("ask: no prompt given")
		}
		return runAsk(prompt)
	},
}

// readAllStdin reads piped input.
func readAllStdin() (string, error) {
	var b strings.Builder
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		b.WriteString(sc.Text())
		b.WriteString("\n")
	}
	return b.String(), sc.Err()
}

func runAsk(prompt string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	pal := tui.NewPalette(false)
	// Plain palette, because this path is the one that gets captured, and escape
	// codes in a captured answer are noise.
	renderer := tui.NewRenderer(os.Stdout, pal)
	session, err := tui.NewSession(tui.SessionOptions{
		Config:    cfg,
		Renderer:  renderer,
		SessionID: "",
		ReadLine:  func(string) (string, error) { return "n", nil },
	})
	if err != nil {
		return err
	}
	return session.RunContext(context.Background(), prompt)
}

func init() {
	root.AddCommand(chatCmd)
	root.AddCommand(chatOnceCmd)
	recordSubcommand(chatCmd)
	recordSubcommand(chatOnceCmd)
}
