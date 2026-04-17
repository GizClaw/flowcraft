package main

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "flowcraft",
	Short: "FlowCraft command-line interface",
	Long: `FlowCraft CLI.

Use "server" to run the HTTP API server in foreground.
Use "start" / "stop" / "status" / "logs" to manage the background server process (Linux).`,
}
