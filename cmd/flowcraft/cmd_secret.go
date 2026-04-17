package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage secrets (reserved for later milestones)",
}

func init() {
	rootCmd.AddCommand(secretCmd)
	secretCmd.AddCommand(secretShowCmd, secretRotateCmd)
}

var secretShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show secret fingerprint (not implemented in M1)",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(os.Stderr, "secret show: not implemented (M1 placeholder)")
		os.Exit(1)
	},
}

var secretRotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Rotate secrets (not implemented in M1)",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(os.Stderr, "secret rotate: not implemented (M1 placeholder)")
		os.Exit(1)
	},
}
