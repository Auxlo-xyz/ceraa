package cmd

import (
	"github.com/spf13/cobra"
	"soraa-agent/internal/telegram"
)

var tgCmd = &cobra.Command{
	Use:   "telegram",
	Short: "Start Telegram bridge (polling) + Soraa Agent harness",
	RunE: func(cmd *cobra.Command, args []string) error {
		return telegram.Run()
	},
}

func init() { root.AddCommand(tgCmd) }
