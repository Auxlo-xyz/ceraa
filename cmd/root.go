package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/emperormk01/soraa-agent/internal/agent"
	"github.com/spf13/cobra"
)

var root = &cobra.Command{
	Use:   "soraa-agent [prompt]",
	Short: "Soraa Agent - coding harness",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return agent.Run(strings.Join(args, " "))
	},
}

var verCmd = &cobra.Command{Use: "version", Run: func(cmd *cobra.Command, args []string) { fmt.Println("soraa-agent 0.1.0") }}

func Execute() {
	root.AddCommand(verCmd)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
