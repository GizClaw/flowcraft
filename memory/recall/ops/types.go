package ops

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	defaultBatchSize    = 32
	defaultIdleInterval = time.Second
	defaultErrorBackoff = 5 * time.Second
	defaultConcurrency  = 1
)

// EventKind identifies one operator event emitted by a Runner.
type EventKind string

const (
	EventDrain     EventKind = "drain"
	EventReadiness EventKind = "readiness"
	EventReconcile EventKind = "reconcile"
)

// MetricsSink receives best-effort structured events. Implementations should
// return quickly; Runner ignores sink errors by design.
type MetricsSink interface {
	ObserveRecallOps(ctx context.Context, event Event)
}

// MetricsSinkFunc adapts a function to MetricsSink.
type MetricsSinkFunc func(ctx context.Context, event Event)

// ObserveRecallOps implements MetricsSink.
func (f MetricsSinkFunc) ObserveRecallOps(ctx context.Context, event Event) {
	if f != nil {
		f(ctx, event)
	}
}

// Event is the stable hook shape for dashboards, logs, and tests.
type Event struct {
	Time      time.Time
	Kind      EventKind
	Scope     recall.Scope
	RuntimeID string
	Duration  time.Duration
	Err       string

	Drain     *ScopeDrainResult
	Readiness *recall.ReadinessReport
	Reconcile *recall.ReconcileResult
}

// Config captures Runner defaults. Most callers should prefer NewRunner
// options; DefaultConfig is available for code that wants to share presets.
type Config struct {
	WorkerID string

	BatchSize           int
	IdleInterval        time.Duration
	ErrorBackoff        time.Duration
	MaxConcurrentScopes int

	DrainSideEffects   bool
	DrainAsyncSemantic bool
	ReadinessOptions   recall.ReadinessOptions
	ReconcileOptions   recall.ReconcileOptions
	ScopeEnumerator    recall.ScopeEnumerator
	Metrics            MetricsSink
	Now                func() time.Time
}

// DefaultConfig returns conservative local-worker defaults.
func DefaultConfig() Config {
	return Config{
		WorkerID:            "recall-ops",
		BatchSize:           defaultBatchSize,
		IdleInterval:        defaultIdleInterval,
		ErrorBackoff:        defaultErrorBackoff,
		MaxConcurrentScopes: defaultConcurrency,
		DrainSideEffects:    true,
		DrainAsyncSemantic:  true,
		Now:                 time.Now,
	}
}

// Option customizes a Runner.
type Option func(*Config)

// WithWorkerID sets the WorkerID passed to claim operations.
func WithWorkerID(id string) Option {
	return func(c *Config) {
		if id != "" {
			c.WorkerID = id
		}
	}
}

// WithBatchSize sets the per-scope claim limit for each drain pass.
func WithBatchSize(n int) Option {
	return func(c *Config) {
		if n > 0 {
			c.BatchSize = n
		}
	}
}

// WithIntervals sets idle and error sleep durations for Run.
func WithIntervals(idle, errorBackoff time.Duration) Option {
	return func(c *Config) {
		if idle > 0 {
			c.IdleInterval = idle
		}
		if errorBackoff > 0 {
			c.ErrorBackoff = errorBackoff
		}
	}
}

// WithMaxConcurrentScopes bounds how many scope drains run at once.
func WithMaxConcurrentScopes(n int) Option {
	return func(c *Config) {
		if n > 0 {
			c.MaxConcurrentScopes = n
		}
	}
}

// WithScopeEnumerator enables runtime-wide operations for drain/readiness.
func WithScopeEnumerator(en recall.ScopeEnumerator) Option {
	return func(c *Config) {
		c.ScopeEnumerator = en
	}
}

// WithDrainKinds controls which queues Run/Drain touches.
func WithDrainKinds(sideEffects, asyncSemantic bool) Option {
	return func(c *Config) {
		c.DrainSideEffects = sideEffects
		c.DrainAsyncSemantic = asyncSemantic
	}
}

// WithReadinessOptions sets readiness thresholds.
func WithReadinessOptions(opts recall.ReadinessOptions) Option {
	return func(c *Config) {
		c.ReadinessOptions = opts
	}
}

// WithReconcileOptions sets default reconcile behavior.
func WithReconcileOptions(opts recall.ReconcileOptions) Option {
	return func(c *Config) {
		c.ReconcileOptions = opts
	}
}

// WithMetrics installs a best-effort event sink.
func WithMetrics(sink MetricsSink) Option {
	return func(c *Config) {
		c.Metrics = sink
	}
}

// WithClock sets the time source used by operator methods.
func WithClock(now func() time.Time) Option {
	return func(c *Config) {
		if now != nil {
			c.Now = now
		}
	}
}

// Target identifies the scopes a run should operate on. Scopes wins over
// RuntimeID; runtime-wide targets require a ScopeEnumerator.
type Target struct {
	RuntimeID string
	Scopes    []recall.Scope
}

// RunOptions configures the continuous drain loop.
type RunOptions struct {
	Target Target
}

// Failure records one failed operation without hiding partial success.
type Failure struct {
	Scope     recall.Scope
	Operation string
	Err       error
}

// Error reports per-scope operation failures.
type Error struct {
	Failures []Failure
}

func (e Error) Error() string {
	if len(e.Failures) == 0 {
		return "recall ops failed"
	}
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		key := f.Scope.PartitionKey()
		if key == "" {
			key = f.Scope.RuntimeID
		}
		if f.Operation != "" {
			key += "/" + f.Operation
		}
		parts = append(parts, fmt.Sprintf("%s: %v", key, f.Err))
	}
	return "recall ops failed: " + strings.Join(parts, "; ")
}

// Unwrap exposes individual causes for errors.Is / errors.As.
func (e Error) Unwrap() []error {
	out := make([]error, 0, len(e.Failures))
	for _, f := range e.Failures {
		if f.Err != nil {
			out = append(out, f.Err)
		}
	}
	return out
}

func validationf(format string, args ...any) error {
	return errdefs.Validationf("recall ops: "+format, args...)
}
