package main

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/spf13/cobra"
)

var cliLogShutdown func(context.Context) error

var rootCmd = &cobra.Command{
	Use:     "flowcraft",
	Short:   "FlowCraft command-line interface",
	Version: cliVersion,
	Long: `FlowCraft CLI.

Use "server" to run the HTTP API server in foreground (Linux only).
Use "start" / "stop" / "status" / "logs" to manage the background server.
Use "config" to view or modify settings.
Use "secret" to manage JWT signing keys.

On Linux the server runs natively.
On macOS it runs inside a vfkit VM.
On Windows it runs inside a WSL2 distribution.`,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		if cmd.Name() == "server" {
			return
		}
		// Suppress the SDK's default-on console sink (v0.1.x compatibility
		// shim) so it doesn't double up with the explicit ConsoleProcessors
		// we register below. Drop the WithLogConsole call once the SDK
		// removes the default in v0.2.0.
		logOpts := []telemetry.LogOption{telemetry.WithLogConsole(false)}
		for _, p := range telemetry.ConsoleProcessors(otellog.SeverityInfo) {
			logOpts = append(logOpts, telemetry.WithLogProcessor(p))
		}
		shutdown, _ := telemetry.InitLog(context.Background(), logOpts...)
		cliLogShutdown = shutdown
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		if cliLogShutdown != nil {
			_ = cliLogShutdown(context.Background())
		}
	},
}
