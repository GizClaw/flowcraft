package commands

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/examples/forge/internal/app"
	"github.com/GizClaw/flowcraft/examples/forge/internal/scenario"
)

func workspaceCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("workspace requires a subcommand\n\n%s", workspaceUsage())
	}
	switch args[0] {
	case "create":
		return workspaceCreateCmd(args[1:])
	case "inspect":
		return workspaceInspectCmd(args[1:])
	case "help", "-h", "--help":
		fmt.Print(workspaceUsage())
		return nil
	default:
		return fmt.Errorf("unknown workspace command %q\n\n%s", args[0], workspaceUsage())
	}
}

func workspaceCreateCmd(args []string) error {
	flags := flag.NewFlagSet("workspace create", flag.ContinueOnError)
	configSource := flags.String("config", "", "raid scenario name or path")
	workspaceDir := flags.String("workspace", "workspace", "workspace directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*configSource) == "" {
		return fmt.Errorf("workspace create requires --config\n\n%s", workspaceUsage())
	}
	ref, err := scenario.Resolve("raids", *configSource)
	if err != nil {
		return err
	}
	if err := scenario.Copy(ref, filepath.Clean(*workspaceDir)); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	fmt.Printf("created workspace %s from scenario %s\n", filepath.Clean(*workspaceDir), *configSource)
	return nil
}

func workspaceInspectCmd(args []string) error {
	flags := flag.NewFlagSet("workspace inspect", flag.ContinueOnError)
	workspaceDir := flags.String("workspace", "workspace", "workspace directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	info, err := app.Inspect(*workspaceDir)
	if err != nil {
		return err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "workspace: %s\n", filepath.Clean(*workspaceDir))
	fmt.Fprintf(&out, "agent: %s (%s)\n", info.AgentID, info.AgentName)
	fmt.Fprintf(&out, "context: %s\n", info.ContextID)
	if info.GenerateModel != "" {
		fmt.Fprintf(&out, "generate_model: %s\n", info.GenerateModel)
	}
	fmt.Fprintf(&out, "memory_enabled: %t\n", info.MemoryEnabled)
	if info.MemoryEnabled {
		fmt.Fprintf(&out, "memory_scope: runtime=%s user=%s agent=%s\n",
			info.MemoryScope.RuntimeID, info.MemoryScope.UserID, info.MemoryScope.AgentID)
		fmt.Fprintf(&out, "memory_recall_top_k: %d\n", info.MemoryTopK)
	}
	fmt.Print(out.String())
	return nil
}

func workspaceUsage() string {
	return strings.TrimLeft(`
Usage:
  forge workspace create --config <raid-scenario> --workspace <dir>
  forge workspace inspect --workspace <dir>
`, "\n")
}

var _ = os.Stdout
