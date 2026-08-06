package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GizClaw/flowcraft/examples/forge/internal/app"
	"github.com/GizClaw/flowcraft/examples/forge/internal/scenario"
	"github.com/GizClaw/flowcraft/examples/forge/internal/tui"
)

func tuiCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("tui requires a subcommand\n\n%s", tuiUsage())
	}
	switch args[0] {
	case "new":
		return tuiNewCmd(args[1:])
	case "resume":
		return tuiResumeCmd(args[1:])
	case "help", "-h", "--help":
		fmt.Print(tuiUsage())
		return nil
	default:
		return fmt.Errorf("unknown tui command %q\n\n%s", args[0], tuiUsage())
	}
}

func tuiNewCmd(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("tui new does not accept arguments yet\n\n%s", tuiUsage())
	}
	raids, err := scenario.List("raids")
	if err != nil {
		return err
	}
	if len(raids) == 0 {
		return fmt.Errorf("no raid configs available")
	}
	items := make([]tui.Item, 0, len(raids))
	for _, raid := range raids {
		items = append(items, tui.Item{Title: raid, Desc: "raid", Value: raid})
	}
	selected, ok, err := tui.RunSelector("Select raid config", items)
	if err != nil || !ok {
		return err
	}
	workspaceDir := filepath.Join("workspaces", selected.Value+"-"+time.Now().Format("20060102_150405"))
	ref, err := scenario.Resolve("raids", selected.Value)
	if err != nil {
		return err
	}
	if err := scenario.Copy(ref, workspaceDir); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	return runTUIWorkspace(workspaceDir)
}

func tuiResumeCmd(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("tui resume does not accept arguments yet\n\n%s", tuiUsage())
	}
	workspaces, err := listWorkspaceOptions()
	if err != nil {
		return err
	}
	if len(workspaces) == 0 {
		return fmt.Errorf("no workspaces under ./workspaces; run `forge tui new` first")
	}
	items := make([]tui.Item, 0, len(workspaces))
	for _, ws := range workspaces {
		items = append(items, tui.Item{Title: filepath.Base(ws), Desc: ws, Value: ws})
	}
	selected, ok, err := tui.RunSelector("Select workspace", items)
	if err != nil || !ok {
		return err
	}
	return runTUIWorkspace(selected.Value)
}

func listWorkspaceOptions() ([]string, error) {
	entries, err := os.ReadDir("workspaces")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join("workspaces", entry.Name(), "deploy.yaml")); err == nil {
			out = append(out, filepath.Join("workspaces", entry.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

func runTUIWorkspace(workspacePath string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := app.Open(ctx, workspacePath)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	program := tea.NewProgram(tui.NewModel(a, workspacePath), tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err = program.Run()
	return err
}

func tuiUsage() string {
	return `Usage:
  forge tui new
  forge tui resume
`
}
