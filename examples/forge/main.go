// Command forge is the runnable local demo for the FlowCraft stack: it assembles
// a deployment document into a host manager, drives it from a TUI or a scripted
// test, and keeps every scenario fact in plain files under scenarios/.
//
// It expects a OPENAI_API_KEY in the environment or in ./.env; the credential
// is only read when the runtime layer that names it is expanded.
package main

import (
	"fmt"
	"os"

	"github.com/GizClaw/flowcraft/examples/forge/internal/commands"
)

func main() {
	if err := loadDotEnv(".env"); err != nil {
		fmt.Fprintln(os.Stderr, "forge:", err)
		os.Exit(1)
	}
	if err := commands.Execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "forge:", err)
		os.Exit(1)
	}
}
