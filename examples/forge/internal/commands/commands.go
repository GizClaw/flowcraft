// Package commands implements the forge CLI surface. Commands only
// parse arguments and delegate to scenario (files), app (runtime), and
// tui — they never assemble the runtime themselves.
package commands

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/examples/forge/internal/scenario"
)

// Execute runs one forge command.
func Execute(args []string) error {
	args = parseGlobalFlags(args)
	if len(args) < 1 {
		printHelp()
		return nil
	}
	switch args[0] {
	case "workspace":
		return workspaceCmd(args[1:])
	case "config":
		return configCmd(args[1:])
	case "tui":
		return tuiCmd(args[1:])
	case "test":
		return testCmd(args[1:])
	case "test-auto":
		return testAutoCmd(args[1:])
	case "help", "-h", "--help":
		return helpCmd(args[1:])
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage())
	}
}

// parseGlobalFlags extracts forge-wide flags that may precede the
// subcommand, such as --scenarios <dir>.
func parseGlobalFlags(args []string) []string {
	if len(args) >= 2 && args[0] == "--scenarios" {
		scenario.SetOverride(args[1])
		return args[2:]
	}
	if len(args) >= 1 && strings.HasPrefix(args[0], "--scenarios=") {
		scenario.SetOverride(strings.TrimPrefix(args[0], "--scenarios="))
		return args[1:]
	}
	return args
}

func printHelp() {
	fmt.Print(usage())
}

func usage() string {
	return strings.TrimLeft(`
Usage:
  forge --scenarios <dir> <command> [flags]
  forge workspace create --config <raid-scenario> --workspace <dir>
  forge workspace inspect --workspace <dir>
  forge config raid list
  forge config persona list
  forge config test list
  forge tui new
  forge tui resume
  forge test -test <name|path> [--timeout 2m]
  forge test-auto --raid <name|path> --persona <name|path> [--timeout 5m]
`, "\n")
}

func configCmd(args []string) error {
	return configCmdWithOutput(args, os.Stdout)
}

func configCmdWithOutput(args []string, w io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("config requires a kind\n\n%s", configUsage())
	}
	switch args[0] {
	case "raid":
		return configKindCmd("raid", args[1:], w, func() ([]string, error) {
			return scenario.List("raids")
		})
	case "persona":
		return configKindCmd("persona", args[1:], w, func() ([]string, error) {
			return scenario.List("personas")
		})
	case "test":
		return configKindCmd("test", args[1:], w, scenario.ListTests)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(w, configUsage())
		return nil
	default:
		return fmt.Errorf("unknown config kind %q\n\n%s", args[0], configUsage())
	}
}

func configKindCmd(kind string, args []string, w io.Writer, list func() ([]string, error)) error {
	if len(args) < 1 {
		return fmt.Errorf("config %s requires a subcommand\n\n%s", kind, configUsage())
	}
	switch args[0] {
	case "list":
		names, err := list()
		if err != nil {
			return fmt.Errorf("list %s configs: %w", kind, err)
		}
		for _, name := range names {
			_, _ = fmt.Fprintln(w, name)
		}
		return nil
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(w, configUsage())
		return nil
	default:
		return fmt.Errorf("unknown config %s command %q\n\n%s", kind, args[0], configUsage())
	}
}

func configUsage() string {
	return strings.TrimLeft(`
Usage:
  forge config raid list
  forge config persona list
  forge config test list
`, "\n")
}

func helpCmd(args []string) error {
	if len(args) == 0 {
		printHelp()
		return nil
	}
	return configCmdWithOutput(args, os.Stdout)
}
