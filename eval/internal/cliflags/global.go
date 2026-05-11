// Package cliflags hosts flags shared across every `eval <suite>`
// subcommand. RegisterPersistent attaches them to the root cobra
// command's PersistentFlags so every leaf suite inherits the same
// set without each suite re-declaring them.
//
// Global flags are deliberately MINIMAL: only the cross-cutting
// concerns (env-file loading, output path, verbose toggle) live here.
// Suite-specific knobs like --dataset, --topk, --domain stay scoped
// to the suite's own RegisterCobra so their semantics and defaults
// belong to whoever owns the suite.
package cliflags

import (
	"encoding/json"
	"os"

	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
)

// jsonMarshalIndent is a 1-line wrapper kept private so the public
// helpers in this file don't drag encoding/json into every caller
// that wants only the global flag set.
func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// Global is the shared flag-state bag. A single instance is created
// in cmd/eval/app and passed to every suite's RegisterCobra so the
// suite can read post-parse values inside its RunE.
type Global struct {
	// EnvFile, when set, points at a .env to load before the
	// subcommand RunE fires. Default empty (rely on the ambient
	// shell + the dotenv autoloader for integration tests).
	EnvFile string

	// OutPath is the canonical "write the JSON Report here" flag.
	// Suites that don't produce a single artefact (e.g. ingest-only
	// utilities) ignore it.
	OutPath string

	// Verbose enables extra log lines. Currently used as a hint by
	// suites that have a quiet / chatty split; reserved for future
	// expansion (per-subcommand log-level overrides).
	Verbose bool

	// Notify holds the notifier flags. Bound once on the root
	// command so every subcommand sees the same notify configuration
	// regardless of where it was supplied on the command line.
	Notify *notify.CLIFlags
}

// RegisterPersistent wires every global flag onto root's
// PersistentFlags. Returns the Global handle so cmd/eval/app can
// stash it for the suite RunEs to read.
func RegisterPersistent(root *cobra.Command) *Global {
	g := &Global{}
	pf := root.PersistentFlags()
	pf.StringVar(&g.EnvFile, "env-file", "", "path to a .env file loaded before the subcommand runs (default: rely on the ambient shell)")
	pf.StringVar(&g.OutPath, "out", "", "write the JSON Report here (default: stdout)")
	pf.BoolVarP(&g.Verbose, "verbose", "v", false, "verbose log output")
	g.Notify = notify.RegisterFlags(pf)
	return g
}

// LoadDotEnv loads the user-specified --env-file (if any) and ALWAYS
// invokes the ambient dotenv autoloader. The autoloader walks up the
// CWD looking for a .env, never overwrites an existing variable, and
// returns silently when nothing is found — safe to call from every
// RunE.
//
// Order matters: explicit --env-file wins because we load it FIRST,
// and the ambient loader respects existing env vars.
func (g *Global) LoadDotEnv() {
	if g.EnvFile != "" {
		env.LoadFile(g.EnvFile)
	}
	env.LoadDotEnv()
}

// WriteReport serialises rep to indented JSON and routes it to either
// --out (when set) or stdout. The "wrote <path>" confirmation line
// goes to stdout when a path was supplied so a pipeline-friendly
// stdout still tells the operator where the artefact landed; on
// stdout-only runs the JSON IS the stdout payload.
//
// Most suites' main RunE used to repeat this 6-line block; lifting
// it here is purely about reducing copy-paste churn — the JSON shape
// of every Report remains owned by its package.
func (g *Global) WriteReport(rep any) error {
	b, err := jsonMarshalIndent(rep)
	if err != nil {
		return err
	}
	if g.OutPath == "" {
		_, err := os.Stdout.Write(append(b, '\n'))
		return err
	}
	if err := os.WriteFile(g.OutPath, b, 0o644); err != nil {
		return err
	}
	_, _ = os.Stdout.WriteString("wrote " + g.OutPath + "\n")
	return nil
}
