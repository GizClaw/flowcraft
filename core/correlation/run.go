package correlation

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
)

// Run is the correlation identity of one run: who is doing the work, on whose
// behalf, and which task the work belongs to.
//
// It exists because the attribute vocabulary already names these facts and no
// carrier existed: a producer could construct attribute.String(correlation.AttrRuntimeID, …)
// by hand, but nothing could hand the pair down to the components that start
// spans. A Runtime sets it once at the top of its context, and every component
// below reads it without knowing that runtime exists.
//
// It is a contracts-tier package for that reason: tool, sandbox, inference and
// agent read the identity without importing the assembly that created it, so the
// carrier cannot live inside core/runtime. The attribute names it renders live
// beside it (attrs.go), so "who the work belongs to" has exactly one home.
//
// The runtime and the host fill RuntimeID, TenantID, SessionID, AgentName and
// Manifest (agent-host restates the runtime id on every turn); the agent fills
// TaskID and Turn; delegation fills the lineage. Nothing here is conversation
// *content*: the fields are the identity a span or log record is correlated
// by. A graph step is execution detail and is a span attribute, not a field
// here.
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
	// TaskID identifies the Task a step belongs to (task.id). It is the
	// checkpoint key, stable across a resume, and empty when the layer that
	// attached the run does not know it yet.
	TaskID string
	// Turn is the 1-based ordinal of the Run call within the session in this
	// process. It is correlation, not identity: it is not persisted and starts
	// over after a restart or eviction.
	Turn int
	// Manifest is the deployment manifest a run executed under.
	Manifest string
	// DelegationDepth is the number of delegation edges above this run; the
	// root caller sees zero. It is lineage rather than identity — a fact about
	// where the run sits in a chain — but it is discovered and read by the same
	// layers, at the same time, so it travels in the same value instead of a
	// second context key.
	DelegationDepth int
	// ChainID identifies the root delegation this run belongs to. Every
	// descendant keeps it; an async job persists it so a resumed child stays in
	// the chain. Empty for a run that was never delegated.
	ChainID string
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
	if run.TaskID != "" {
		merged.TaskID = run.TaskID
	}
	if run.Turn != 0 {
		merged.Turn = run.Turn
	}
	if run.Manifest != "" {
		merged.Manifest = run.Manifest
	}
	if run.DelegationDepth != 0 {
		merged.DelegationDepth = run.DelegationDepth
	}
	if run.ChainID != "" {
		merged.ChainID = run.ChainID
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
//
// Delegation depth and the chain id are deliberately not rendered: they are not
// the identity a span is correlated by, and the delegation layer already carries
// them on its own records (Outcome, Job). Rendering them here would put a
// delegation fact on every span of a nested run.
func buildAttrs(run Run) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 7)
	if run.RuntimeID != "" {
		attrs = append(attrs, attribute.String(AttrRuntimeID, run.RuntimeID))
	}
	if run.TenantID != "" {
		attrs = append(attrs, attribute.String(AttrTenantID, run.TenantID))
	}
	if run.SessionID != "" {
		attrs = append(attrs, attribute.String(AttrSessionID, run.SessionID))
	}
	if run.AgentName != "" {
		attrs = append(attrs, attribute.String(AttrAgentName, run.AgentName))
	}
	if run.TaskID != "" {
		attrs = append(attrs, attribute.String(AttrTaskID, run.TaskID))
	}
	if run.Turn > 0 {
		attrs = append(attrs, attribute.Int(AttrTurn, run.Turn))
	}
	if run.Manifest != "" {
		attrs = append(attrs, attribute.String(AttrManifestID, run.Manifest))
	}
	return attrs
}

// Attrs returns the correlation attributes to attach to a span or log record.
//
// It returns nil when nothing is attached, so a caller writes
// span.SetAttributes(correlation.Attrs(ctx)...) without a nil check, and a
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
