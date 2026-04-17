package main

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "flowcraft",
	Short: "FlowCraft command-line interface",
	Long: `FlowCraft CLI.

Use "server" to run the HTTP API server in foreground (Linux only).
Use "start" / "stop" / "status" / "logs" to manage the background server.
Use "config" to view or modify settings.
Use "secret" to manage JWT signing keys.

On Linux the server runs natively. On macOS it runs inside a Lima VM.
On Windows it runs inside a WSL2 distribution.`,
}
