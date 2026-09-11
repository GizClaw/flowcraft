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
	"sort"
	"strings"
	"sync/atomic"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

// App owns one built runtime.
type App struct {
	info            Info
	dir             string
	rt              *runtime.Runtime
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
	AgentID   string
	AgentName string
	ContextID string
	Speakers  map[string]string
}

// Open parses deploy.yaml from the workspace and assembles the
// runtime exactly like any other sdkx/deploy + sdkx/runtime consumer.
func Open(ctx context.Context, workspaceDir string) (*App, error) {
	raw, err := os.ReadFile(filepath.Join(workspaceDir, "deploy.yaml"))
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
	a := &App{info: info, dir: workspaceDir}
	rt, err := buildRuntimeFromDocument(ctx, a, raw)
	if err != nil {
		return nil, err
	}
	a.rt = rt
	return a, nil
}

// Close shuts the runtime down.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
	var errs []error
	if a.rt != nil {
		errs = append(errs, a.rt.Close())
	}
	return errors.Join(errs...)
}

// Info returns the workspace metadata.
func (a *App) Info() Info {
	if a == nil {
		return Info{}
	}
	return a.info
}

// Models lists every text-generation target the inference assembly exposes,
// as "provider/name" strings the router accepts as a model hint. The TUI
// offers these next to its automatic option, so a selection can never name a
// target the deployment does not actually serve.
func (a *App) Models() []string {
	if a == nil || a.rt == nil {
		return nil
	}
	value, ok := a.rt.Resource("infer")
	if !ok {
		return nil
	}
	assembly, ok := value.(*inference.Assembly)
	if !ok {
		return nil
	}
	var models []string
	for _, provider := range assembly.Providers() {
		for _, model := range provider.Models {
			if !servesText(model.Descriptor.Capabilities.Outputs) {
				continue
			}
			models = append(models, provider.ID+"/"+model.Descriptor.ID.Name)
		}
	}
	sort.Strings(models)
	return models
}

// servesText reports whether an output modality set makes a model a
// text-generation target; image, audio, and embedding models are not
// selectable as chat backends.
func servesText(outputs []message.PartKind) bool {
	for _, kind := range outputs {
		if kind == message.PartText {
			return true
		}
	}
	return false
}

// SpeakerLabel returns the user-facing label for a graph node id from
// the scenario's speakers.yaml, or "" when the scenario does not
// declare one.
func (a *App) SpeakerLabel(nodeID string) string {
	if a == nil || a.info.Speakers == nil {
		return ""
	}
	return a.info.Speakers[nodeID]
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
	return out.String()
}

// TurnOptions carries the per-turn inference preferences a host selects.
// Zero values mean "auto": the graph's routing policy picks the model, and
// the provider applies its own default reasoning effort. The values ride the
// engine as board vars, so a graph opts in with ${board:model} /
// ${board:think_effort} references.
type TurnOptions struct {
	// Model is a "provider/name" target or empty for automatic routing.
	Model string
	// Think is a canonical reasoning level (minimal/low/medium/high/xhigh)
	// or empty to leave the effort to the provider default.
	Think string
}

// inputs renders the options as board vars. Auto values are omitted rather
// than set empty, so a graph's "${board:x:}" default applies.
func (o TurnOptions) inputs() map[string]any {
	inputs := map[string]any{}
	if o.Model != "" {
		inputs["model"] = o.Model
	}
	if o.Think != "" {
		inputs["think_effort"] = o.Think
	}
	if len(inputs) == 0 {
		return nil
	}
	return inputs
}

// RunTurn sends one user text through the session manager and returns
// the assembled result.
func (a *App) RunTurn(
	ctx context.Context,
	text string,
	sink session.SinkSpec,
	opts TurnOptions,
) (*agent.Result, error) {
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
		Inputs:  opts.inputs(),
	}, sink)
	if err != nil {
		return nil, err
	}
	return turn.Wait(ctx)
}
