package main

import (
	"context"
	"os"
	"runtime"

	"github.com/GizClaw/flowcraft/cmd/flowcraft/machine"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/spf13/cobra"
	otellog "go.opentelemetry.io/otel/log"
)

// Set at build time via: go build -ldflags "-X main.cliVersion=..."
var cliVersion = "dev"

func resolveMachine() machine.Machine {
	return machine.NewMachine(cliVersion)
}

func init() {
	rootCmd.AddCommand(startCmd, stopCmd, statusCmd, logsCmd)
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the FlowCraft background server",
	Long: `Start the FlowCraft server as a background process.

On Linux the server runs natively. On macOS it launches a Lima VM.
On Windows it runs inside a WSL2 distribution.
Runtime images are downloaded automatically on first use.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		if runtime.GOOS != "linux" {
			latest, needsUpdate, err := machine.CheckVersionMismatch(ctx, cliVersion)
			if err == nil && needsUpdate {
				telemetry.Warn(ctx, "cli: newer runtime version available",
					otellog.String("latest", latest), otellog.String("current", cliVersion))
			}
		}

		return resolveMachine().Start(ctx)
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the FlowCraft background server",
	RunE: func(cmd *cobra.Command, args []string) error {
		return resolveMachine().Stop(context.Background())
	},
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show FlowCraft server status",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		st, err := resolveMachine().Status(ctx)
		if err != nil {
			return err
		}
		telemetry.Info(ctx, "cli: server status",
			otellog.Bool("running", st.Running),
			otellog.Int("pid", st.PID),
			otellog.Bool("healthz_ok", st.HealthzOK))
		return nil
	},
}

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Print FlowCraft server logs",
	RunE: func(cmd *cobra.Command, args []string) error {
		return resolveMachine().Logs(context.Background(), os.Stdout)
	},
}
