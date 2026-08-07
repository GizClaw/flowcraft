// Package eval runs LoCoMo and LongMemEval benchmarks against the flowcraft
// memory implementation.
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if err := runCLI(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "memory-eval:", err)
		os.Exit(1)
	}
}

func runCLI(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "run":
		return runCommand(ctx, args[1:])
	case "convert":
		return convertCommand(ctx, args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (want run or convert)", args[0])
	}
}
