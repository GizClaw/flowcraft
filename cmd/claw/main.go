package main

import (
	"fmt"
	"os"

	"github.com/GizClaw/flowcraft/cmd/claw/cli"
)

func main() {
	if err := cli.Execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
