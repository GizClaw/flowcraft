package main

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/GizClaw/flowcraft/cmd/flowcraft/machine"
	"github.com/spf13/cobra"
)

const cliVersion = "0.5.0"

func resolveMachine() machine.Machine {
	return machine.Resolve(cliVersion)
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
				fmt.Printf("Note: a newer runtime version %s is available (CLI is %s).\n", latest, cliVersion)
				fmt.Println("The updated image will be downloaded automatically.")
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
		st, err := resolveMachine().Status(context.Background())
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "running=%v pid=%d healthz_ok=%v\n", st.Running, st.PID, st.HealthzOK)
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
