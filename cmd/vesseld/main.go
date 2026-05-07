package main

import (
	"fmt"
	"os"

	"github.com/GizClaw/flowcraft/cmd/vesseld/cli"
)

// main is the thin entry point. All flag parsing, sub-command
// dispatch, and exit-code policy live in cmd/vesseld/cli so the
// runtime / fleet / api packages can be exercised from tests
// without paying the cost of executing the binary.
func main() {
	if err := cli.Execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
