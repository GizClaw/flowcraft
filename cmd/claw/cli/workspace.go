package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/claw"
)

func workspaceCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("workspace requires a subcommand\n\n%s", usage())
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
	flags := flag.NewFlagSet("workspace create", flag.ExitOnError)
	configSource := flags.String("config", "", "config template path or embedded example name")
	workspaceDir := flags.String("workspace", "workspace", "workspace directory")
	workspaceDirTypo := flags.String("worksapce", "", "workspace directory")
	flags.Parse(args)
	if strings.TrimSpace(*workspaceDirTypo) != "" {
		workspaceDir = workspaceDirTypo
	}

	if strings.TrimSpace(*configSource) == "" {
		return fmt.Errorf("workspace create requires --config\n\n%s", workspaceUsage())
	}
	if err := WriteConfig(templateFS, *configSource, *workspaceDir); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	fmt.Printf("created workspace %s from config %s\n", *workspaceDir, *configSource)
	return nil
}

func workspaceInspectCmd(args []string) error {
	flags := flag.NewFlagSet("workspace inspect", flag.ExitOnError)
	workspaceDir := flags.String("workspace", "workspace", "workspace directory")
	workspaceDirTypo := flags.String("worksapce", "", "workspace directory")
	flags.Parse(args)
	if strings.TrimSpace(*workspaceDirTypo) != "" {
		workspaceDir = workspaceDirTypo
	}

	text, err := inspectWorkspace(*workspaceDir)
	if err != nil {
		return err
	}
	fmt.Print(text)
	return nil
}

func inspectWorkspace(workspaceDir string) (string, error) {
	app, err := openApp(workspaceDir)
	if err != nil {
		return "", err
	}
	cfg := app.Config()
	if err := app.Close(); err != nil {
		return "", fmt.Errorf("close workspace before inspect: %w", err)
	}
	var out strings.Builder
	writeWorkspaceInspect(&out, workspaceDir, cfg)
	if cfg.Memory.Enabled {
		fmt.Fprintln(&out)
		fmt.Fprint(&out, inspectMemoryMetadataSection(context.Background(), "memory", workspaceDir))
	}
	if cfg.Memory.Enabled && cfg.Memory.Retrieval.Backend == "bbh" {
		fmt.Fprintln(&out)
		fmt.Fprint(&out, inspectBBHWorkspaceSection(context.Background(), "memory", workspaceDir))
	}
	return out.String(), nil
}

func openApp(workspaceDir string) (*claw.Claw, error) {
	ws, err := workspace.NewLocalWorkspace(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("open workspace: %w", err)
	}
	app, err := claw.New(ws)
	if err != nil {
		return nil, fmt.Errorf("create claw: %w", err)
	}
	return app, nil
}

func writeWorkspaceInspect(w io.Writer, workspaceDir string, cfg claw.Config) {
	fmt.Fprintf(w, "workspace: %s\n", filepath.Clean(workspaceDir))
	fmt.Fprintf(w, "memory_root: %s\n", cfg.Workspace.MemoryRoot)
	fmt.Fprintf(w, "state_root: %s\n", cfg.Workspace.StateRoot)
	fmt.Fprintf(w, "agent: %s (%s)\n", cfg.Agent.ID, cfg.Agent.Name)
	fmt.Fprintf(w, "chat_model: %s\n", cfg.Models.Chat)
	if cfg.Models.Extractor != "" {
		fmt.Fprintf(w, "extractor_model: %s\n", cfg.Models.Extractor)
	}
	if cfg.Models.Embedder != "" {
		fmt.Fprintf(w, "embedder_model: %s\n", cfg.Models.Embedder)
	}
	fmt.Fprintf(w, "memory_enabled: %t\n", cfg.Memory.Enabled)
	if cfg.Memory.Enabled {
		scope := cfg.Memory.Scope
		fmt.Fprintf(w, "memory_scope: runtime=%s user=%s agent=%s\n", scope.RuntimeID, scope.UserID, scope.AgentID)
		fmt.Fprintf(w, "memory_backend: %s\n", cfg.Memory.Retrieval.Backend)
		fmt.Fprintf(w, "memory_recall_top_k: %d\n", cfg.Memory.Recall.TopK)
		fmt.Fprintf(w, "memory_write_mode: %s\n", cfg.Memory.Write.Mode)
	}
	writeNamedModels(w, "llm_models", cfg.Models.LLM)
	embeddings := cfg.Models.Embedding
	if len(cfg.Models.Embeddings) > 0 {
		embeddings = cfg.Models.Embeddings
	}
	writeNamedModels(w, "embedding_models", embeddings)
}

func writeNamedModels(w io.Writer, label string, models map[string]claw.ModelConfig) {
	if len(models) == 0 {
		return
	}
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintf(w, "%s:", label)
	for _, name := range names {
		model := models[name]
		fmt.Fprintf(w, " %s=%s/%s", name, model.Provider, model.Model)
	}
	fmt.Fprintln(w)
}

func workspaceUsage() string {
	return strings.TrimLeft(`
Usage:
  claw workspace create --config <name|path> --workspace <dir>
  claw workspace inspect --workspace <dir>
`, "\n")
}
