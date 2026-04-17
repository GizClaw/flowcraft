package main

import (
	"context"
	"fmt"
	"os"

	"github.com/GizClaw/flowcraft/cmd/flowcraft/machine"
	"github.com/spf13/cobra"
)

var lifecycleMachine = machine.NewNative()

func init() {
	rootCmd.AddCommand(startCmd, stopCmd, statusCmd, logsCmd)
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the M1 background server (Linux)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return lifecycleMachine.Start(context.Background())
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the M1 background server (Linux)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return lifecycleMachine.Stop(context.Background())
	},
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show M1 background server status (Linux)",
	RunE: func(cmd *cobra.Command, args []string) error {
		st, err := lifecycleMachine.Status(context.Background())
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "running=%v pid=%d healthz_ok=%v\n", st.Running, st.PID, st.HealthzOK)
		return nil
	},
}

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Print M1 server log file (Linux)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return lifecycleMachine.Logs(context.Background(), os.Stdout)
	},
}
