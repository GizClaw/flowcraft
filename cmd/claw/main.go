package main

import (
	"bufio"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/claw"
)

//go:embed examples/*.yaml
var exampleFS embed.FS

func main() {
	if len(os.Args) < 2 {
		printHelp()
		return
	}
	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "create":
		createCmd(os.Args[2:])
	case "examples":
		examplesCmd()
	case "help", "-h", "--help":
		printHelp()
	default:
		if strings.HasPrefix(os.Args[1], "-") {
			runCmd(os.Args[1:])
			return
		}
		exitf("unknown command %q\n\n%s", os.Args[1], usage())
	}
}

func runCmd(args []string) {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	workspaceDir := flags.String("workspace", "workspace", "workspace directory")
	configRoot := flags.String("config", "config", "workspace config directory")
	contextID := flags.String("context", "default", "conversation context id")
	flags.Parse(args)

	ws, err := workspace.NewLocalWorkspace(*workspaceDir)
	if err != nil {
		exitf("open workspace: %v", err)
	}
	cfg, err := Load(context.Background(), ws, *configRoot)
	if err != nil {
		exitf("load config: %v", err)
	}
	app, err := claw.New(ws, claw.WithConfig(cfg))
	if err != nil {
		exitf("create claw: %v", err)
	}
	defer func() { _ = app.Close() }()

	fmt.Printf("claw workspace=%s context=%s\n", *workspaceDir, *contextID)
	fmt.Println("type /exit to quit")

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		if text == "/exit" || text == "/quit" {
			break
		}
		if err := runTurn(context.Background(), app, *contextID, text); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
	}
	if err := scanner.Err(); err != nil {
		exitf("read input: %v", err)
	}
}

func createCmd(args []string) {
	flags := flag.NewFlagSet("create", flag.ExitOnError)
	exampleName := flags.String("example", "chat", "example name")
	workspaceDir := flags.String("workspace", "workspace", "workspace directory")
	flags.Parse(args)

	if err := WriteExample(exampleFS, *exampleName, *workspaceDir); err != nil {
		exitf("create workspace: %v", err)
	}
	fmt.Printf("created workspace %s from example %s\n", *workspaceDir, *exampleName)
}

func examplesCmd() {
	examples, err := listExamples()
	if err != nil {
		exitf("list examples: %v", err)
	}
	for _, name := range examples {
		fmt.Println(name)
	}
}

func listExamples() ([]string, error) {
	entries, err := exampleFS.ReadDir("examples")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			switch ext {
			case ".yaml", ".yml", ".json":
				out = append(out, strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func runTurn(ctx context.Context, app *claw.Claw, contextID, text string) error {
	resp, err := app.RoundTrip(contextID, claw.Request{Context: ctx, Text: text})
	if err != nil {
		return err
	}
	for {
		ev, err := resp.Next()
		if errors.Is(err, io.EOF) {
			fmt.Println()
			return nil
		}
		if err != nil {
			return err
		}
		switch ev.Type {
		case claw.EventToken:
			fmt.Print(ev.Content)
		case claw.EventToolCall:
			fmt.Printf("\n[tool call] %s %v\n", ev.Name, ev.Arguments)
		case claw.EventToolResult:
			fmt.Printf("\n[tool result] %s\n", ev.Content)
		case claw.EventError:
			fmt.Println()
			return fmt.Errorf("%s", ev.Err)
		case claw.EventResult:
			// Tokens already carried the visible answer.
		}
	}
}

func printHelp() {
	fmt.Print(usage())
}

func usage() string {
	return strings.TrimLeft(`
Usage:
  claw run --workspace <dir> [--config <dir>] [--context <id>]
  claw create --example <name> --workspace <dir>
  claw examples
  claw help
`, "\n")
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
