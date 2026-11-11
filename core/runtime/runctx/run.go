package runctx

import (
	"context"

	"go.opentelemetry.io/otel/attribute"

	"github.com/GizClaw/flowcraft/core/telemetry"
)

// Run is the correlation identity of one assembled runtime.
//
// It exists because the attribute vocabulary already names these facts and no
// carrier existed: a producer could construct attribute.String(telemetry.AttrRuntimeID, …)
// by hand, but nothing could hand the pair down to the components that start
// spans. A Runtime sets it once at the top of its context, and every component
// below reads it without knowing that runtime exists — which is the only reason
// the carrier lives here rather than in core/runtime: tool and inference must be
// able to read it without importing that package.
//
// The runtime fills RuntimeID/TenantID; a layer that owns a session adds
// SessionID/AgentName/Turn/Manifest. Nothing here is conversation *content*:
// the fields are the identity a span or log record is correlated by.
type Run struct {
	// RuntimeID identifies the assembled runtime (agent.runtime.id).
	RuntimeID string
	// TenantID identifies the tenant on whose behalf the work happens. Empty
	// for a single-tenant deployment, which stays a valid configuration.
	TenantID string
	// SessionID identifies the session (the container of tasks).
	SessionID string
	// AgentName names the deployed agent.
	AgentName string
	// Turn is the conversation turn a step belongs to.
	Turn int
	// Manifest is the deployment manifest a run executed under.
	Manifest string
}

type runKey struct{}

// attached pairs the identity with the attributes it renders to. Building them
// on every span was two allocations per span for a value that cannot change
// between WithRun and the span it decorates.
type attached struct {
	run   Run
	attrs []attribute.KeyValue
}

// WithRun attaches run identity to ctx.
//
// A nested call overrides only the fields it sets, so a layer that knows the
// tenant keeps the runtime id it inherited: the two facts are discovered at
// different depths, and neither should erase the other.
func WithRun(ctx context.Context, run Run) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	merged := stored(ctx)
	if run.RuntimeID != "" {
		merged.RuntimeID = run.RuntimeID
	}
	if run.TenantID != "" {
		merged.TenantID = run.TenantID
	}
	if run.SessionID != "" {
		merged.SessionID = run.SessionID
	}
	if run.AgentName != "" {
		merged.AgentName = run.AgentName
	}
	if run.Turn != 0 {
		merged.Turn = run.Turn
	}
	if run.Manifest != "" {
		merged.Manifest = run.Manifest
	}
	return context.WithValue(ctx, runKey{}, attached{run: merged, attrs: buildAttrs(merged)})
}

// From returns the run identity attached to ctx.
func From(ctx context.Context) (Run, bool) {
	run := stored(ctx)
	return run, run != Run{}
}

// stored reads the attached value, so merging and reporting do not repeat the
// type assertion.
func stored(ctx context.Context) Run {
	if ctx == nil {
		return Run{}
	}
	att, _ := ctx.Value(runKey{}).(attached)
	return att.run
}

// buildAttrs renders the identity once, when it is attached.
func buildAttrs(run Run) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 6)
	if run.RuntimeID != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrRuntimeID, run.RuntimeID))
	}
	if run.TenantID != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrTenantID, run.TenantID))
	}
	if run.SessionID != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrSessionID, run.SessionID))
	}
	if run.AgentName != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrAgentName, run.AgentName))
	}
	if run.Turn > 0 {
		attrs = append(attrs, attribute.Int(telemetry.AttrTurn, run.Turn))
	}
	if run.Manifest != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrManifestID, run.Manifest))
	}
	return attrs
}

// Attrs returns the correlation attributes to attach to a span or log record.
//
// It returns nil when nothing is attached, so a caller writes
// span.SetAttributes(runctx.Attrs(ctx)...) without a nil check, and a
// hand-built context stays as cheap as before.
func Attrs(ctx context.Context) []attribute.KeyValue {
	if ctx == nil {
		return nil
	}
	att, ok := ctx.Value(runKey{}).(attached)
	if !ok || len(att.attrs) == 0 {
		return nil
	}
	// The cap keeps a caller's append from writing into the shared slice.
	return att.attrs[:len(att.attrs):len(att.attrs)]
}
