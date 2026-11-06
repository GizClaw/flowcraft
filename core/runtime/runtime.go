package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/skill"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/engine"

	"go.opentelemetry.io/otel/attribute"
)

// Runtime is one assembled agent substrate: the model access, the tools with
// their instances, the sandbox runner, the file boundary, the skill index and
// the journal, plus the ownership that closes them.
//
// It holds nothing that belongs to a conversation. Two Runtimes in one process
// share no state, and neither can close the other's resources.
type Runtime struct {
	id  string
	cfg Config

	tree     *fsx.Tree
	runner   sandbox.Runner
	journal  sandbox.FileJournal
	provider inference.Provider
	router   *route.Router
	assembly *engine.Assembly
	registry *tool.Registry
	skills   *skill.Index
	sessions io.Closer

	closers []io.Closer

	once sync.Once
	err  error
}

// Option adjusts how a Runtime is built.
type Option func(*options)

type options struct {
	drivers   []Driver
	selectors []Selector
}

// WithSelectors declares the selection strategies YAML may name. Pure Go callers
// never need this: they pass a Selector value to route.Policy directly.
func WithSelectors(selectors ...Selector) Option {
	return func(o *options) { o.selectors = append(o.selectors, selectors...) }
}

// WithDrivers declares the driver table. A name in inference.driver that is not
// in the table fails construction, and the error lists what is available.
func WithDrivers(drivers ...Driver) Option {
	return func(o *options) { o.drivers = append(o.drivers, drivers...) }
}

// New builds one runtime from cfg and starts it.
//
// Every resource is created here and closed by [Runtime.Close]; nothing is
// inherited from the caller. A failure anywhere closes what was already built,
// so a partially constructed runtime never leaks a process or a handle.
func New(ctx context.Context, cfg Config, opts ...Option) (*Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	settings := options{}
	for _, opt := range opts {
		opt(&settings)
	}
	drivers, err := driverTable(settings.drivers)
	if err != nil {
		return nil, err
	}
	selectors, err := selectorTable(settings.selectors)
	if err != nil {
		return nil, err
	}

	rt := &Runtime{id: newID(), cfg: cfg}
	if err := rt.build(ctx, drivers, selectors); err != nil {
		_ = rt.Close()
		return nil, err
	}
	rt.logAssembled(ctx)
	return rt, nil
}

// ID identifies this runtime in spans and logs.
func (r *Runtime) ID() string { return r.id }

// Context attaches this runtime's identity to ctx, so every span and log record
// created below it carries agent.runtime.id without the components knowing this
// package exists. A tenant already present is preserved, and a host that knows
// the tenant adds it with [runctx.WithRun] on the same context.
func (r *Runtime) Context(ctx context.Context) context.Context {
	return runctx.WithRun(ctx, runctx.Run{RuntimeID: r.id})
}

// Config is the configuration this runtime was built from.
func (r *Runtime) Config() Config { return r.cfg }

// Provider is the model access; the zero value when no driver was configured.
func (r *Runtime) Provider() inference.Provider { return r.provider }

// Tools is the registry holding every wired tool instance.
func (r *Runtime) Tools() *tool.Registry { return r.registry }

// Router is the failover dispatcher over the declared models, nil when the
// runtime declares none.
func (r *Runtime) Router() *route.Router { return r.router }

// DefaultModel is the first declared model: the caller's default when a request
// names none. Its Provider is the target name, so a caller may pin one; leaving
// it empty lets routing choose.
func (r *Runtime) DefaultModel() model.ModelRef {
	if len(r.cfg.Inference.Models) == 0 {
		return model.ModelRef{}
	}
	entry := r.cfg.Inference.Models[0]
	return model.ModelRef{
		ID:      model.ModelID{Provider: entry.Driver, Name: entry.Model},
		Profile: entry.Profile,
	}
}

// Assembly is the tool surface: catalog, executor, and — when tools.dynamic is
// set — the policy session views are created from.
func (r *Runtime) Assembly() *engine.Assembly { return r.assembly }

// NewToolSession returns a fresh visibility view over the tool catalog.
//
// The view is per round and therefore not stored: it remembers which tools the
// model discovered, which went idle, and what the current round may show. A
// Runtime holding one would make two conversations share that state.
func (r *Runtime) NewToolSession() engine.Session { return r.assembly.NewSession() }

// Files is the file boundary the tools work inside.
func (r *Runtime) Files() *fsx.Tree { return r.tree }

// Runner is the process boundary.
func (r *Runtime) Runner() sandbox.Runner { return r.runner }

// Journal is the file journal, nil when the runtime did not request one.
func (r *Runtime) Journal() sandbox.FileJournal { return r.journal }

// Skills is the skill index, nil when no roots were configured.
func (r *Runtime) Skills() *skill.Index { return r.skills }

// Close releases everything this runtime built, in reverse construction order:
// sessions and tools first, then the journal, then the runner, then the file
// tree. Close is idempotent and returns the first error it met.
func (r *Runtime) Close() error {
	r.once.Do(func() {
		var first error
		for i := len(r.closers) - 1; i >= 0; i-- {
			if err := r.closers[i].Close(); err != nil && first == nil {
				first = err
			}
		}
		r.closers = nil
		r.err = first
	})
	return r.err
}

// logAssembled records what was granted, because a resolved permission that
// nobody can see is a permission nobody reviews. The layer names ride in the
// message when the host built this from layers.
func (r *Runtime) logAssembled(ctx context.Context) {
	attrs := append(runctx.Attrs(r.Context(ctx)),
		attribute.String("sandbox.backend", r.cfg.Sandbox.Backend),
		attribute.StringSlice("files.roots", r.cfg.Files.Roots),
		attribute.StringSlice("sandbox.writes", r.cfg.Sandbox.Writes),
		attribute.String("sandbox.net", r.cfg.Sandbox.Net.Mode),
		attribute.Int("tools", r.registry.Len()),
		attribute.Bool("tools.dynamic", r.cfg.Tools.Dynamic != nil),
	)
	if len(r.cfg.Tools.MCP.Servers) > 0 {
		attrs = append(attrs, attribute.Int("tools.mcp.servers", len(r.cfg.Tools.MCP.Servers)))
	}
	if len(r.cfg.Inference.Models) > 0 {
		attrs = append(attrs, attribute.Int("inference.models", len(r.cfg.Inference.Models)))
	}
	if r.journal != nil {
		attrs = append(attrs, attribute.Bool("sandbox.journal", true))
	}
	telemetry.Info(ctx, "agent runtime assembled", attrs...)
}

func newID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// A runtime without an id is still better than no runtime; the id only
		// groups telemetry.
		return "runtime"
	}
	return "rt-" + hex.EncodeToString(buf[:])
}

// errorf is the package's error shape for constructor problems.
func errorf(format string, args ...any) error {
	return errdefs.Validationf(format, args...)
}
