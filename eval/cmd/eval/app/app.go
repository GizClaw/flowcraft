// Package app wires the root cobra command for the unified `eval`
// binary. Every suite registers itself here via its public
// RegisterCobra constructor; the root owns the shared persistent
// flags (env-file, output path, verbose, notify) so leaf suites
// inherit them uniformly.
//
// Pattern intentionally mirrors animus-test
// (tapdoki/go/animus/cmd/animus-test/app) so anyone familiar with
// that codebase can navigate this one immediately.
package app

import (
	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/beir"
	"github.com/GizClaw/flowcraft/eval/internal/cliflags"
	knowledgeqa "github.com/GizClaw/flowcraft/eval/knowledge"
	"github.com/GizClaw/flowcraft/eval/locomo"
	"github.com/GizClaw/flowcraft/eval/longmemeval"
	"github.com/GizClaw/flowcraft/eval/simpleqa"
	"github.com/GizClaw/flowcraft/eval/taubench"
)

// Global is the shared flag handle every suite RunE consults.
var Global *cliflags.Global

var rootCmd = &cobra.Command{
	Use:   "eval",
	Short: "FlowCraft evaluation suite — retrieval, memory, factuality, tool-use",
	Long: `eval is the unified entry point for every benchmark under eval/.

Run a suite by name; each suite has its own --help with full flag detail.

Memory / dialog:
  eval locomo                LoCoMo-10 dialog memory benchmark
  eval longmemeval           LongMemEval converter (uses the locomo runner)

Retrieval:
  eval knowledge             hand-curated knowledge-base regression
  eval beir                  BEIR-format public retrieval datasets

Factuality:
  eval simpleqa              SimpleQA short-form factuality + calibration

Tool use:
  eval taubench              τ-bench (single-shot + multi-turn dialog)

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

	// Each suite owns its own RegisterCobra so flag changes stay
	// confined to that suite's package. The root never references a
	// suite's private types — it just hands over a *cobra.Command
	// and the *cliflags.Global handle.
	locomo.RegisterCobra(rootCmd, Global)
	longmemeval.RegisterCobra(rootCmd, Global)
	knowledgeqa.RegisterCobra(rootCmd, Global)
	beir.RegisterCobra(rootCmd, Global)
	simpleqa.RegisterCobra(rootCmd, Global)
	taubench.RegisterCobra(rootCmd, Global)
}

// Execute runs the root command. main.go handles the os.Exit dance.
func Execute() error {
	return rootCmd.Execute()
}
