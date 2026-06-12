package cli

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const tuiCloseTimeout = 2 * time.Second

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
	raids, err := listTUIRaidOptions()
	if err != nil {
		return err
	}
	items := make([]tuiSelectItem, 0, len(raids))
	for _, raid := range raids {
		items = append(items, tuiSelectItem{
			Title: raid.Name,
			Desc:  raid.Source,
			Value: raid.Name,
		})
	}
	selected, ok, err := runTUISelector("Select raid config", items)
	if err != nil || !ok {
		return err
	}
	workspace, err := createOrOpenTUIWorkspaceFromRaid(selected.Value)
	if err != nil {
		return err
	}
	return runTUIWorkspace(workspace.Path)
}

func tuiResumeCmd(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("tui resume does not accept arguments yet\n\n%s", tuiUsage())
	}
	workspaces, err := listTUIWorkspaceOptions()
	if err != nil {
		return err
	}
	items := make([]tuiSelectItem, 0, len(workspaces))
	for _, workspace := range workspaces {
		desc := workspace.Path
		if workspace.LastOpenedAt != "" {
			desc = "last_opened=" + workspace.LastOpenedAt + "  " + workspace.Path
		}
		items = append(items, tuiSelectItem{
			Title: workspace.Name,
			Desc:  desc,
			Value: workspace.Path,
		})
	}
	selected, ok, err := runTUISelector("Select workspace", items)
	if err != nil || !ok {
		return err
	}
	if err := touchTUIWorkspace(selected.Value); err != nil {
		return err
	}
	return runTUIWorkspace(selected.Value)
}

func runTUIWorkspace(workspacePath string) (err error) {
	app, err := openApp(workspacePath)
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), tuiCloseTimeout)
		defer cancel()
		if closeErr := app.CloseContext(ctx); closeErr != nil && err == nil && ctx.Err() == nil {
			err = closeErr
		}
	}()
	_, err = tea.NewProgram(newTUIModel(app, workspacePath), tea.WithAltScreen()).Run()
	return err
}

func tuiUsage() string {
	return `Usage:
  claw tui new
  claw tui resume
`
}
