// Package app wires the root cobra command for the `eval` binary.
// The root owns the shared persistent flags (env-file, output path,
// verbose, notify) so benchmark commands inherit them uniformly.
//
// Pattern intentionally mirrors animus-test
// (tapdoki/go/animus/cmd/animus-test/app) so anyone familiar with
// that codebase can navigate this one immediately.
package app

import (
	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/internal/cliflags"
	"github.com/GizClaw/flowcraft/eval/simpleqa"
)

// Global is the shared flag handle every suite RunE consults.
var Global *cliflags.Global

var rootCmd = &cobra.Command{
	Use:   "eval",
	Short: "FlowCraft evaluation harness",
	Long: `eval is the entry point for FlowCraft's maintained evaluation harnesses.

Available suites:
  eval simpleqa              SimpleQA short-form factuality + calibration

Notification + global behaviour is controlled by --notify-* / --env-file
flags shown on every subcommand.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		// Load .env so suite-level RunEs see the resolved env. We
		// do this in PersistentPreRun so the order is deterministic:
		// global flags parsed → env loaded → suite RunE.
		Global.LoadDotEnv()
		return nil
	},
}

func init() {
	Global = cliflags.RegisterPersistent(rootCmd)

	// The suite owns its RegisterCobra so flag changes stay confined
	// to the suite package. The root just hands over shared globals.
	simpleqa.RegisterCobra(rootCmd, Global)
}

// Execute runs the root command. main.go handles the os.Exit dance.
func Execute() error {
	return rootCmd.Execute()
}
