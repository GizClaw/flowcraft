// Command validate-config checks one FlowCraft document without building
// anything.
//
// It is the same loading code the platform runs — `deploy.Loader` for a
// deployment document and its runtime layers, `graph.Parse`/`nodes.Validate` for
// a graph document — so what it accepts is what the host will accept. It is not a
// build: no provider, sandbox or tool is constructed, so a settings block's
// semantics, its credentials and the reachability of an endpoint are the host's
// business, not this tool's.
package main

import (
	"flag"
	"fmt"
	"os"
)

const (
	exitOK    = 0
	exitError = 1
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("validate-config", flag.ContinueOnError)
	kind := fs.String("type", "deploy", "deploy|graph")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: validate-config [--type deploy|graph] <file>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitError
	}
	path := fs.Arg(0)

	var err error
	switch *kind {
	case "deploy":
		err = validateDeploy(path)
	case "graph":
		err = validateGraph(path)
	default:
		fmt.Fprintf(os.Stderr, "unknown --type %q\n", *kind)
		return exitError
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] %v\n", class(err), err)
		return exitError
	}
	return exitOK
}
