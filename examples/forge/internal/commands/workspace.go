package commands

import (
	"flag"
	"fmt"
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
	infos, missing, err := app.Inspect(*workspaceDir)
	if err != nil {
		return err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "workspace: %s\n", filepath.Clean(*workspaceDir))
	fmt.Fprintf(&out, "agents: %d\n", len(infos))
	for _, info := range infos {
		// A director plans or referees and has no line of its own, so the header
		// says which mode the agent is in before the fields say anything else.
		if info.Director {
			fmt.Fprintf(&out, "\n%s (director)\n", info.Agent)
		} else {
			fmt.Fprintf(&out, "\n%s (speaker: %s)\n", info.Agent, info.Label)
		}
		fmt.Fprintf(&out, "  engine: %s", info.Engine)
		if info.Graph != "" {
			fmt.Fprintf(&out, "  graph: %s", info.Graph)
		}
		fmt.Fprintln(&out)
		if info.Runtime != "" {
			fmt.Fprintf(&out, "  runtime layer: %s\n", info.Runtime)
		}
		fmt.Fprintf(&out, "  session: %s\n", info.Session)
		if len(info.Models) > 0 {
			fmt.Fprintf(&out, "  models: %s\n", strings.Join(info.Models, ", "))
		}
		if len(info.Tools) > 0 {
			fmt.Fprintf(&out, "  tools: %s\n", strings.Join(info.Tools, ", "))
		}
		if len(info.Delegates) > 0 {
			fmt.Fprintf(&out, "  delegates: %s\n", strings.Join(info.Delegates, ", "))
		}
		if len(info.Hooks) > 0 {
			fmt.Fprintf(&out, "  hooks: %s\n", strings.Join(info.Hooks, ", "))
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(&out, "missing env: %s\n", strings.Join(missing, ", "))
	} else {
		fmt.Fprintln(&out, "missing env: none")
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
