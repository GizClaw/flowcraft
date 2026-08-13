// Package-level usage plumbing: the forge app decorates the runtime's
// base host factory so every LLM call's token usage is mirrored onto
// the App for TUI display. The runtime itself remains the owner of
// usage aggregation; this is a read-only mirror.
package app

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

// UsageSnapshot is a point-in-time cumulative token accounting.
type UsageSnapshot struct {
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	Calls            int64
}

// Since returns the usage accumulated between two snapshots.
func (u UsageSnapshot) Since(before UsageSnapshot) UsageSnapshot {
	return UsageSnapshot{
		InputTokens:      u.InputTokens - before.InputTokens,
		OutputTokens:     u.OutputTokens - before.OutputTokens,
		TotalTokens:      u.TotalTokens - before.TotalTokens,
		ReasoningTokens:  u.ReasoningTokens - before.ReasoningTokens,
		CacheReadTokens:  u.CacheReadTokens - before.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens - before.CacheWriteTokens,
		Calls:            u.Calls - before.Calls,
	}
}

// addUsage mirrors one inference usage report onto the app counters.
func (a *App) addUsage(u inference.Usage) {
	a.usageIn.Add(u.InputTokens)
	a.usageOut.Add(u.OutputTokens)
	a.usageTot.Add(u.TotalTokens)
	if v := u.Output.ReasoningTokens; v != nil {
		a.usageReason.Add(*v)
	}
	if v := u.Input.CacheReadTokens; v != nil {
		a.usageCacheRead.Add(*v)
	}
	if v := u.Input.CacheWriteTokens; v != nil {
		a.usageCacheWrite.Add(*v)
	}
	a.usageCalls.Add(1)
}

// usageHostFactory wraps the runtime's base host factory so
// ReportUsage reaches the App while everything else delegates to the
// base host (event publishing, interrupts, ask-user, checkpoints).
type usageHostFactory struct {
	base session.HostFactory
	app  *App
}

func (f *usageHostFactory) NewHost(ctx context.Context, request session.HostRequest) (agent.Host, error) {
	base, err := f.base.NewHost(ctx, request)
	if err != nil {
		return nil, err
	}
	return agent.HostFuncs{
		Inner: base,
		ReportUsageFn: func(_ context.Context, usage inference.Usage) error {
			f.app.addUsage(usage)
			return base.ReportUsage(ctx, usage)
		},
	}, nil
}

// usageHostDecorator adapts the app mirror to the runtime's host
// factory decorator contract: it must hand back a factory that wraps
// the provided base factory, never one that replaces it.
func usageHostDecorator(a *App) runtime.HostFactoryDecorator {
	return func(base session.HostFactory) (session.HostFactory, error) {
		if base == nil {
			return nil, errors.New("usage host decorator: runtime base host factory is nil")
		}
		return &usageHostFactory{base: base, app: a}, nil
	}
}
