package config

import (
	"context"
	"fmt"
	"slices"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// BuiltinKind is the default source kind that resolves hand-written Go
// tools from the Builder's builtin catalog:
//
//	sources:
//	  - kind: builtin
//	    spec: {tools: [search, exec]}
//
// It is registered by default; external source kinds are opt-in.
const BuiltinKind = "builtin"

// Source is an attachment that populates a tool.Registry from
// somewhere outside the process — an MCP server, a remote agent, a
// plugin host. It deliberately does not implement tool.Catalog: the
// Registry the Builder creates is the catalog, and a source's job is to
// write into it so that externally-provided tools and hand-written Go
// tools are indistinguishable to every downstream consumer.
//
// A source is a live resource. Attach connects and discovers; Close
// releases whatever Attach acquired (child processes, HTTP sessions)
// and retracts the tools it registered. The Builder never closes a
// source it created — see Builder.Build's return value — because the
// host's lifetime, not the Executor's, is what governs.
type Source interface {
	// Attach connects and registers the source's tools into registry.
	// It must be synchronous: when it returns nil, the registry is
	// complete for this source.
	Attach(ctx context.Context, registry *tool.Registry) error
	// Close releases the source and unregisters its tools.
	Close() error
}

// SourceFactory instantiates one Source from its own opaque spec, nil
// when the entry declared no spec. Decode it with [DecodeSpec],
// mirroring MiddlewareFactory: an unknown field is a typo that should
// fail at build time, not silently drop a server.
type SourceFactory func(ctx context.Context, in sdkconfig.Input) (Source, error)

// SourceEntry is one attachment declared in the document: a factory
// kind plus its opaque spec. Spec stays an undecoded JSON subtree so
// each factory owns its own schema.
type SourceEntry struct {
	Kind string            `json:"kind"`
	Spec *sdkconfig.Opaque `json:"spec,omitempty"`
}

// RegisterSourceFactory adds (or replaces) a factory for kind. Empty
// kinds and nil factories are programming bugs.
//
// No external kind is registered by default. A source pulls in an
// external dependency (the MCP SDK, an RPC client), so the host opts in
// explicitly rather than every consumer of this package paying for
// every integration:
//
//	builder.RegisterSourceFactory("mcp", mcpconfig.SourceFactory)
func (b *Builder) RegisterSourceFactory(kind string, factory SourceFactory) {
	if kind == "" {
		panic("config.RegisterSourceFactory: kind is empty")
	}
	if factory == nil {
		panic(fmt.Sprintf("config.RegisterSourceFactory: factory for kind %q is nil", kind))
	}
	b.sourceFactories[kind] = factory
}

type builtinSpec struct {
	Tools []string `json:"tools"`
}

// builtinSourceFactory resolves the named hand-written Go tools from
// the Builder's catalog. The source itself is inert: the tools are
// already compiled into the program, so Close has nothing to release.
func (b *Builder) builtinSourceFactory(_ context.Context, in sdkconfig.Input) (Source, error) {
	spec, err := DecodeSpec[builtinSpec](in.Settings)
	if err != nil {
		return nil, err
	}
	if len(spec.Tools) == 0 {
		return nil, errdefs.Validationf(
			"builtin source: tools must name at least one tool")
	}
	return &builtinSource{
		catalog: b.builtins,
		names:   append([]string(nil), spec.Tools...),
	}, nil
}

type builtinSource struct {
	catalog map[string]tool.Tool
	names   []string
}

func (s *builtinSource) Attach(_ context.Context, registry *tool.Registry) error {
	for _, name := range s.names {
		registered, ok := s.catalog[name]
		if !ok {
			return errdefs.Validationf(
				"builtin source: unknown builtin tool %q", name)
		}
		registry.Register(registered)
	}
	return nil
}

func (s *builtinSource) Close() error { return nil }

// buildSources instantiates and attaches every declared source, in
// document order.
//
// On the first failure every already-attached source is closed before
// returning, so a partially-built document never leaves child processes
// running or half a server's tools in the registry. That is why Build
// returns the attached sources on success: the caller must close them,
// and there is no other handle to them.
func (b *Builder) buildSources(ctx context.Context, entries []SourceEntry, registry *tool.Registry) ([]Source, error) {
	attached := make([]Source, 0, len(entries))
	closeAttached := func() {
		for _, a := range slices.Backward(attached) {
			_ = a.Close()
		}
	}
	for i, entry := range entries {
		factory, ok := b.sourceFactories[entry.Kind]
		if !ok {
			closeAttached()
			return nil, errdefs.Validation(fmt.Errorf(
				"tool config sources[%d]: unknown kind %q "+
					"(register it with Builder.RegisterSourceFactory)", i, entry.Kind,
			))
		}
		source, err := factory(ctx, sdkconfig.Input{Settings: entry.Spec})
		if err != nil {
			closeAttached()
			return nil, fmt.Errorf(
				"tool config sources[%d] (%s): %w", i, entry.Kind, err,
			)
		}
		if source == nil {
			closeAttached()
			return nil, errdefs.Validation(fmt.Errorf(
				"tool config sources[%d] (%s): factory returned a nil source",
				i, entry.Kind,
			))
		}
		if err := source.Attach(ctx, registry); err != nil {
			_ = source.Close()
			closeAttached()
			return nil, fmt.Errorf(
				"tool config sources[%d] (%s): attach: %w", i, entry.Kind, err,
			)
		}
		attached = append(attached, source)
	}
	return attached, nil
}
