package main

import (
	"fmt"

	"github.com/GizClaw/flowcraft/internal/paths"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(configCmd)
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show where FlowCraft reads config.yaml",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(paths.ConfigFile())
	},
}
