// Command eval is the unified entry point for every FlowCraft
// evaluation suite. It dispatches to suite-specific subcommands
// (`eval locomo`, `eval beir`, `eval taubench`, …) so a single
// binary covers the full eval/ tree.
//
// See `eval --help` for the suite list and per-subcommand help.
package main

import (
	"fmt"
	"os"

	"github.com/GizClaw/flowcraft/eval/cmd/eval/app"
)

func main() {
	if err := app.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
