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

	logsCmd.Flags().BoolP("crash", "c", false, "read the stdout/stderr crash log instead of the server log (Linux only)")
	logsCmd.Flags().Bool("vm", false, "read the vfkit VM console log instead of the server log (macOS only)")
	logsCmd.Flags().IntP("tail", "n", 0, "print only the last N lines (0 = print all)")
	logsCmd.MarkFlagsMutuallyExclusive("crash", "vm")
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
	Long: `Print FlowCraft server logs.

By default reads the structured server log (log.file.path, default
~/.flowcraft/data/logs/server.log) — written by the OTel pipeline with
size-based rotation. On macOS this file lives on the host's shared data
directory and is the same inode the in-VM server writes to.

Use --crash (Linux) to read the stdout/stderr capture file for panics
and any output emitted before telemetry initialized. Use --vm (macOS)
to read the vfkit console log when the guest never reached the server
stage. Use -n/--tail to limit output to the last N lines.

Configure rotation via log.file.{max_size_mb,max_backups,max_age_days,compress}
in config.yaml; see ` + "`flowcraft config`" + ` for current values.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := machine.LogsOptions{Source: machine.LogsServer}
		if c, _ := cmd.Flags().GetBool("crash"); c {
			opts.Source = machine.LogsCrash
		}
		if v, _ := cmd.Flags().GetBool("vm"); v {
			opts.Source = machine.LogsVM
		}
		if n, _ := cmd.Flags().GetInt("tail"); n > 0 {
			opts.TailLines = n
		}
		return resolveMachine().Logs(context.Background(), os.Stdout, opts)
	},
}
