// Package cli is the sub-command dispatcher for the vesseld
// binary. The actual sub-command implementations land in run.go /
// validate.go / plan.go as the runtime / api / fleet packages
// stabilise; for now Execute carries enough logic that `vesseld
// version` and `vesseld --help` work and unknown sub-commands fail
// with a helpful error.
package cli

import (
	"fmt"
)

// Version is set at link time via -ldflags "-X main.Version=...".
// Default is "dev" so developers running `go build` get a clear
// signal they are not on a tagged release.
var Version = "dev"

// Execute runs the requested sub-command and returns a Go error on
// failure. The main wrapper translates the error into a stderr
// print + exit(1); keeping the boundary inside Execute means tests
// can call it directly.
func Execute(args []string) error {
	if len(args) == 0 {
		return printHelp()
	}
	switch args[0] {
	case "version", "-v", "--version":
		fmt.Println(Version)
		return nil
	case "help", "-h", "--help":
		return printHelp()
	case "run":
		return cmdRun(args[1:])
	case "validate":
		return cmdValidate(args[1:])
	case "plan":
		return cmdPlan(args[1:])
	default:
		return fmt.Errorf("vesseld: unknown subcommand %q (try `vesseld help`)", args[0])
	}
}

func printHelp() error {
	fmt.Println(`vesseld — FlowCraft Vessel orchestration daemon

Usage:
  vesseld run      --config DIR [-R]    start the daemon
  vesseld validate --config DIR [-R]    schema + ref check, no IO
  vesseld plan     --config DIR [-R]    print resolved Plan (secrets redacted)
  vesseld version                       module versions
  vesseld help                          this message

Configuration is loaded from one or more --config inputs. Each
input is a file or a directory; directories are scanned non-
recursively unless -R / --recursive is set.`)
	return nil
}

// cmdRun / cmdValidate / cmdPlan are wired in their own files so
// each sub-command's flag parsing + business logic stays small
// and individually testable.
