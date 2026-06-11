// Command eval is the entry point for FlowCraft's maintained evaluation
// harnesses. Today it dispatches to the SimpleQA suite.
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
