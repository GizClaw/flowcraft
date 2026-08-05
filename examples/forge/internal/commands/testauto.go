package commands

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/GizClaw/flowcraft/examples/forge/internal/app"
	"github.com/GizClaw/flowcraft/examples/forge/internal/scenario"
)

func testAutoCmd(args []string) error {
	flags := flag.NewFlagSet("test-auto", flag.ContinueOnError)
	raidSource := flags.String("raid", "", "raid scenario name or path")
	personaSource := flags.String("persona", "", "persona scenario name or path")
	timeout := flags.Duration("timeout", 5*time.Minute, "maximum duration per simulated turn")
	turns := flags.Int("turns", 3, "number of simulated turns")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *raidSource == "" || *personaSource == "" {
		return fmt.Errorf("test-auto requires --raid and --persona\n\n%s", usage())
	}
	if *turns <= 0 {
		return fmt.Errorf("test-auto requires --turns > 0")
	}

	raidRef, err := scenario.Resolve("raids", *raidSource)
	if err != nil {
		return err
	}
	personaRef, err := scenario.Resolve("personas", *personaSource)
	if err != nil {
		return err
	}

	outputDir := testRunDir(*raidSource+"-"+*personaSource, time.Now())
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create simulation output directory: %w", err)
	}
	workspacePath := filepath.Join(outputDir, "workspace")
	if err := scenario.Copy(raidRef, workspacePath); err != nil {
		return fmt.Errorf("create simulation workspace: %w", err)
	}
	if err := applyPersona(personaRef, workspacePath); err != nil {
		return fmt.Errorf("apply persona: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout*time.Duration(*turns)+time.Minute)
	defer cancel()
	a, err := app.Open(ctx, workspacePath)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	fmt.Printf("simulating raid=%s persona=%s turns=%d\n", *raidSource, *personaSource, *turns)
	var log strings.Builder
	for i := 1; i <= *turns; i++ {
		text := simulatedTurnInput(i)
		fmt.Printf("--- turn %d: %s\n", i, text)
		collector := &textCollectorSink{}
		result, err := a.RunTurn(ctx, text, collector.spec())
		if err != nil {
			return fmt.Errorf("turn %d: %w", i, err)
		}
		reply := collector.builder.String()
		if reply == "" && result != nil && len(result.Messages) > 0 {
			reply = result.Messages[len(result.Messages)-1].Content.Text()
		}
		fmt.Printf("assistant: %s\n\n", reply)
		fmt.Fprintf(&log, "=== Turn %d ===\nuser: %s\nassistant: %s\n\n", i, text, reply)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "chat_log.txt"), []byte(log.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote simulation %s\n", outputDir)
	return nil
}

// applyPersona overlays the persona's native agents section onto the
// workspace deploy.yaml and installs the persona graph. The resulting
// workspace remains a plain deploy/runtime document set.
func applyPersona(ref scenario.Ref, workspacePath string) error {
	agentRaw, err := scenario.ReadFile(ref, "agent.yaml")
	if err != nil {
		return err
	}
	var personaAgents map[string]any
	if err := yaml.Unmarshal(agentRaw, &personaAgents); err != nil {
		return fmt.Errorf("persona agent.yaml: %w", err)
	}
	deployPath := filepath.Join(workspacePath, "deploy.yaml")
	deployRaw, err := os.ReadFile(deployPath)
	if err != nil {
		return err
	}
	var document map[string]any
	if err := yaml.Unmarshal(deployRaw, &document); err != nil {
		return err
	}
	document["agents"] = personaAgents["agents"]
	merged, err := yaml.Marshal(document)
	if err != nil {
		return err
	}
	if err := os.WriteFile(deployPath, merged, 0o644); err != nil {
		return err
	}
	graphRaw, err := scenario.ReadFile(ref, "graphs/persona.json")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(workspacePath, "graphs"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workspacePath, "graphs", "persona.json"), graphRaw, 0o644)
}

func simulatedTurnInput(turn int) string {
	switch turn {
	case 1:
		return "你好，开始吧。"
	case 2:
		return "介绍一下你自己。"
	default:
		return "你还记得我们聊过什么吗？"
	}
}
