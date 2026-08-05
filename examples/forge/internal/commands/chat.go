package commands

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/examples/forge/internal/app"
)

// chatCmd runs an interactive REPL backed by the workspace runtime.
func chatCmd(args []string) error {
	workspaceDir, contextID, err := parseChatFlags(args)
	if err != nil {
		return err
	}
	ctx := context.Background()
	a, err := app.Open(ctx, workspaceDir)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	info := a.Info()
	if contextID != "" {
		info.ContextID = contextID
	}
	fmt.Printf("forge chat: agent=%s context=%s (ctrl+d to quit)\n", info.AgentName, info.ContextID)
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
		if strings.EqualFold(text, "/quit") || strings.EqualFold(text, "/exit") {
			break
		}
		result, err := a.RunTurn(ctx, text, terminalSink(os.Stdout))
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			continue
		}
		if result != nil && result.Status != "completed" {
			fmt.Println()
			if detail := resultErrorDetail(result); detail != "" {
				fmt.Printf("[%s: %s]\n", result.Status, detail)
			} else {
				fmt.Printf("[%s]\n", result.Status)
			}
		}
	}
	return scanner.Err()
}

func parseChatFlags(args []string) (workspaceDir, contextID string, err error) {
	workspaceDir = "workspace"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--workspace":
			if i+1 < len(args) {
				i++
				workspaceDir = args[i]
			}
		case "--context":
			if i+1 < len(args) {
				i++
				contextID = args[i]
			}
		default:
			return "", "", fmt.Errorf("unknown flag %q", args[i])
		}
	}
	return workspaceDir, contextID, nil
}
