// Package app assembles one runnable forge application from a
// workspace directory: it parses the native deploy.yaml, registers the
// deployment/runtime factories, and runs turns through the session
// manager. It owns no CLI, UI, or scenario concerns.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	runtimecore "github.com/GizClaw/flowcraft/sdkx/runtime"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
)

// App owns one built runtime plus the memory System captured via the
// forge.debug integration (the Runtime API does not expose the
// deployment result).
type App struct {
	info            Info
	dir             string
	rt              *runtimecore.Runtime
	tools           *tool.Registry
	memory          sdkmemory.ContextProvider
	toolCalls       atomic.Int64
	usageIn         atomic.Int64
	usageOut        atomic.Int64
	usageTot        atomic.Int64
	usageReason     atomic.Int64
	usageCacheRead  atomic.Int64
	usageCacheWrite atomic.Int64
	usageCalls      atomic.Int64
}

// Info is the small metadata read out of the native documents for
// inspection and TUI display.
type Info struct {
	AgentID       string
	AgentName     string
	ContextID     string
	GenerateModel string
	MemoryEnabled bool
	MemoryScope   Scope
	MemoryTopK    int
}

// Scope mirrors the memory scope seed in memory.yaml.
type Scope struct {
	RuntimeID string
	UserID    string
	AgentID   string
}

// Open parses deploy.yaml from the workspace and assembles the
// runtime exactly like any other sdkx/deploy + sdkx/runtime consumer.
func Open(ctx context.Context, workspaceDir string) (*App, error) {
	raw, err := os.ReadFile(filepath.Join(workspaceDir, "deploy.yaml"))
	if err != nil {
		return nil, err
	}
	raw, err = absolutizeDeployment(raw, workspaceDir)
	if err != nil {
		return nil, err
	}
	doc, err := deploy.Parse(raw)
	if err != nil {
		return nil, err
	}
	info, err := inspectDocument(workspaceDir, doc)
	if err != nil {
		return nil, err
	}
	if err := requireProviderCredential(workspaceDir); err != nil {
		return nil, err
	}
	a := &App{info: info, dir: workspaceDir, tools: tool.NewRegistry()}
	rt, err := buildRuntimeFromDocument(ctx, a, doc)
	if err != nil {
		return nil, err
	}
	a.rt = rt
	return a, nil
}

// Close shuts the runtime down.
func (a *App) Close() error {
	if a == nil || a.rt == nil {
		return nil
	}
	return a.rt.Close()
}

// Info returns the workspace metadata.
func (a *App) Info() Info {
	if a == nil {
		return Info{}
	}
	return a.info
}

// Memory returns the deployed memory context provider, nil when
// disabled.
func (a *App) Memory() sdkmemory.ContextProvider {
	if a == nil {
		return nil
	}
	return a.memory
}

// ToolCalls returns the simulated tool execution counter.
func (a *App) ToolCalls() int64 {
	if a == nil {
		return 0
	}
	return a.toolCalls.Load()
}

// Usage returns the cumulative token usage reported by LLM calls since
// the app opened. Callers snapshot before and after a turn to derive
// per-turn totals; the runtime host owns aggregation, and the app only
// mirrors it for UI surfaces.
func (a *App) Usage() UsageSnapshot {
	if a == nil {
		return UsageSnapshot{}
	}
	return UsageSnapshot{
		InputTokens:      a.usageIn.Load(),
		OutputTokens:     a.usageOut.Load(),
		TotalTokens:      a.usageTot.Load(),
		ReasoningTokens:  a.usageReason.Load(),
		CacheReadTokens:  a.usageCacheRead.Load(),
		CacheWriteTokens: a.usageCacheWrite.Load(),
		Calls:            a.usageCalls.Load(),
	}
}

// Inspect reads workspace metadata without building the runtime.
func Inspect(workspaceDir string) (Info, error) {
	raw, err := os.ReadFile(filepath.Join(workspaceDir, "deploy.yaml"))
	if err != nil {
		return Info{}, err
	}
	doc, err := deploy.Parse(raw)
	if err != nil {
		return Info{}, err
	}
	return inspectDocument(workspaceDir, doc)
}

// Describe renders workspace metadata for inspect and debug output.
func (a *App) Describe() string {
	info := a.Info()
	var out strings.Builder
	fmt.Fprintf(&out, "workspace: %s\n", a.dir)
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
	return out.String()
}

// RunTurn sends one user text through the session manager and returns
// the assembled result.
func (a *App) RunTurn(ctx context.Context, text string, sink session.SinkSpec) (*agent.Result, error) {
	if a == nil || a.rt == nil {
		return nil, errors.New("forge app is not open")
	}
	lease, err := a.rt.Sessions().Open(ctx, session.Key{
		AgentID:   a.info.AgentID,
		ContextID: a.info.ContextID,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = lease.Close() }()
	turn, err := lease.Session().Start(ctx, agent.Request{
		Message: message.NewTextMessage(message.RoleUser, text),
	}, sink)
	if err != nil {
		return nil, err
	}
	return turn.Wait(ctx)
}
