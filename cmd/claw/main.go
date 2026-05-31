package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/claw"
)

func main() {
	workspaceDir := flag.String("workspace", "workspace", "workspace directory")
	contextID := flag.String("context", "default", "conversation context id")
	flag.Parse()

	ws, err := workspace.NewLocalWorkspace(*workspaceDir)
	if err != nil {
		exitf("open workspace: %v", err)
	}
	cfg, err := claw.LoadConfig(context.Background(), ws)
	if err != nil {
		exitf("load config: %v", err)
	}
	cfg.ExpandEnv()
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

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
