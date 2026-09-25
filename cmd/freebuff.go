package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"ceraa/internal/freebuff"
	"github.com/spf13/cobra"
)

var freebuffCmd = &cobra.Command{
	Use:   "freebuff",
	Short: "Manage the native Freebuff login",
}

// freebuffLoginCmd starts the device login and blocks until the browser
// approval lands, so Freebuff can be authenticated without the Telegram bot.
var freebuffLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Start the Freebuff browser login and wait for approval",
	RunE: func(cmd *cobra.Command, args []string) error {
		manager := freebuff.NewLoginManager(nil)
		flow, err := manager.Start(context.Background())
		if err != nil {
			return fmt.Errorf("start freebuff login: %w", err)
		}
		fmt.Println(flow.ResultMessage())
		reader := bufio.NewReader(os.Stdin)
		fmt.Println("Press Enter once you approved the login in the browser (Ctrl-C to abort).")
		go func() {
			_, _ = reader.ReadString('\n')
		}()
		deadline := time.Now().Add(freebuff.PollTimeout + 30*time.Second)
		for time.Now().Before(deadline) {
			status, message := flow.StatusSnapshot()
			switch status {
			case "succeeded":
				fmt.Println("✅ Freebuff login successful.")
				return nil
			case "failed":
				return fmt.Errorf("freebuff login failed: %s", message)
			}
			time.Sleep(500 * time.Millisecond)
		}
		return fmt.Errorf("freebuff login did not complete in time")
	},
}

var freebuffStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the stored Freebuff identity and verify it live",
	RunE: func(cmd *cobra.Command, args []string) error {
		identity, err := freebuff.NewStore().Status(context.Background())
		if err != nil {
			if identity == "" {
				return err
			}
			fmt.Printf("Freebuff account: %s\nwarning: %v\n", identity, err)
			return nil
		}
		fmt.Printf("Freebuff account: %s\n", identity)
		return nil
	},
}

var freebuffLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Clear the stored Freebuff credential",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := freebuff.NewStore().Logout(context.Background()); err != nil {
			return err
		}
		fmt.Println("✅ Freebuff credential cleared.")
		return nil
	},
}

func init() {
	freebuffCmd.AddCommand(freebuffLoginCmd)
	freebuffCmd.AddCommand(freebuffStatusCmd)
	freebuffCmd.AddCommand(freebuffLogoutCmd)
}

// freebuffSummary renders a one-line Freebuff status for doctor output. It
// never contacts the network, so doctor stays fast and offline-safe.
func freebuffSummary() string {
	credential, err := freebuff.NewStore().Resolve(context.Background())
	if err != nil {
		return "freebuff: not authenticated (run: ceraa freebuff login)"
	}
	identity := strings.TrimSpace(credential.User.Email)
	if identity == "" {
		identity = strings.TrimSpace(credential.User.ID)
	}
	if identity == "" {
		identity = "unknown account"
	}
	return fmt.Sprintf("freebuff: authenticated as %s via %s", identity, strings.TrimSpace(credential.BaseURL))
}
