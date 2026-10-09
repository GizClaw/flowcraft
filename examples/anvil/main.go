// Command anvil is a small host application built on the craft module.
//
// It assembles one Craft from a craft.yaml definition, one compile-time
// capability and one plugin directory, then walks the craft lifecycle a
// real shell (desktop, HTTP, headless) drives: scanning plugins, opening
// keyed runtimes, calling tools, reloading a runtime, hot-plugging a
// plugin and shutting down.
//
// Everything is local and deterministic — no model provider, no network
// access, no credentials. Run it from this directory:
//
//	go run .
//
// See README.md for the tour step by step, and docs/guides/craft.md for
// the contract it exercises.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "anvil:", err)
		os.Exit(1)
	}
}

func run() error {
	var opts options
	flag.StringVar(&opts.definition, "definition", "craft.yaml",
		"path to the craft definition")
	flag.StringVar(&opts.dataDir, "data", ".anvil",
		"writable directory for plugin state, notes and layers")
	timeout := flag.Duration("timeout", 3*time.Minute,
		"bound for the whole tour")
	flag.Parse()
	opts.out = os.Stdout

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	a, err := newApp(opts)
	if err != nil {
		return err
	}
	return a.tour(ctx)
}
