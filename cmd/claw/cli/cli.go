package cli

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed examples
var templateFS embed.FS

func Execute(args []string) error {
	if err := loadDotEnv(".env"); err != nil {
		return fmt.Errorf("load .env: %w", err)
	}
	if err := ensureClawConfigDir(); err != nil {
		return fmt.Errorf("sync configs: %w", err)
	}
	if len(args) < 1 {
		printHelp()
		return nil
	}
	switch args[0] {
	case "workspace":
		return workspaceCmd(args[1:])
	case "config":
		return configCmd(args[1:])
	case "serve":
		return serveCmd(args[1:])
	case "tui":
		return tuiCmd(args[1:])
	case "test-auto":
		return testAutoCmd(args[1:])
	case "test":
		return testCmd(args[1:])
	case "help", "-h", "--help":
		return helpCmd(args[1:])
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage())
	}
}

func printHelp() {
	fmt.Print(usage())
}

func usage() string {
	return strings.TrimLeft(`
Usage:
  claw workspace create --config <name|path> --workspace <dir>
  claw workspace inspect --workspace <dir>
  claw config raid list
  claw config persona list
  claw config test list
  claw serve --workspace <dir> [--addr 127.0.0.1:8787]
  claw tui new
  claw tui resume
  claw test-auto --raid <name|path> --persona <name|path> [--timeout 5m]
  claw test -test <test/raid/case|raid/case|path> [--timeout 2m]
`, "\n")
}

func recoverCommand(errp *error) {
	if recovered := recover(); recovered != nil {
		*errp = fmt.Errorf("panic: %v", recovered)
	}
}
