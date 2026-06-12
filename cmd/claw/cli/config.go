package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func configCmd(args []string) error {
	return configCmdWithOutput(args, os.Stdout)
}

func configCmdWithOutput(args []string, w io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("config requires a kind\n\n%s", configUsage())
	}
	switch args[0] {
	case "raid":
		return configKindCmd("raid", args[1:], w, listRaids)
	case "persona":
		return configKindCmd("persona", args[1:], w, listPersonas)
	case "test":
		return configKindCmd("test", args[1:], w, listTests)
	case "help", "-h", "--help":
		fmt.Fprint(w, configUsage())
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
			fmt.Fprintln(w, name)
		}
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(w, configUsage())
		return nil
	default:
		return fmt.Errorf("unknown config %s command %q\n\n%s", kind, args[0], configUsage())
	}
}

func configUsage() string {
	return strings.TrimLeft(`
Usage:
  claw config raid list
  claw config persona list
  claw config test list
`, "\n")
}
