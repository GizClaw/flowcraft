package main

import (
	"context"
	"fmt"
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
	rootCmd.AddCommand(startCmd, stopCmd, statusCmd, logsCmd, resetCmd, webCmd)
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the FlowCraft background server",
	Long: `Start the FlowCraft server as a background process.

On Linux the server runs natively. On macOS it launches a vfkit VM.
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

var resetCmd = &cobra.Command{
	Use:   "reset <machine|data|all>",
	Short: "Stop the server and delete FlowCraft state",
	Long: `Reset stops the running server and removes selected FlowCraft state.

Scopes:
  machine   Remove VM / machine files only; preserves user data and config.
            Next "start" will re-download the image and re-provision.
  data      Remove user data (database, uploads) only; preserves VM.
  all       Remove the entire ~/.flowcraft directory — full factory reset.`,
	Args:      cobra.ExactArgs(1),
	ValidArgs: []string{"machine", "data", "all"},
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		var scope machine.ResetScope
		switch args[0] {
		case "machine":
			scope = machine.ResetMachine
		case "data":
			scope = machine.ResetData
		case "all":
			scope = machine.ResetAll
		default:
			return fmt.Errorf("unknown scope %q; use machine, data, or all", args[0])
		}

		force, _ := cmd.Flags().GetBool("force")
		if !force {
			telemetry.Warn(ctx, "reset: this will delete FlowCraft state — use --force to confirm",
				otellog.String("scope", args[0]))
			return nil
		}

		if err := resolveMachine().Reset(ctx, scope); err != nil {
			return err
		}
		telemetry.Info(ctx, "reset: done", otellog.String("scope", args[0]))
		return nil
	},
}

func init() {
	resetCmd.Flags().Bool("force", false, "skip confirmation and perform the reset")
}

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Open the FlowCraft web UI in the default browser",
	RunE: func(cmd *cobra.Command, args []string) error {
		return resolveMachine().OpenWeb(context.Background())
	},
}

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Print FlowCraft server logs",
	RunE: func(cmd *cobra.Command, args []string) error {
		return resolveMachine().Logs(context.Background(), os.Stdout)
	},
}
