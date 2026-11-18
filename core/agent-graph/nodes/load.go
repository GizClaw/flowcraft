package nodes

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
)

// Option configures a graph opened for a runtime.
type Option func(*options)

type options struct {
	kinds    []graph.NodeKind
	builders []func(Runtime) ([]graph.NodeKind, error)
	decoders map[string]inference.ExtensionDecoder
	blobs    graph.BlobStore
}

// WithRuntimeKinds adds node kinds built from the runtime this graph is opened
// for.
//
// A kind that dispatches through the substrate - the script node hands a tool call
// to the runtime's assembly - cannot be built by the host, because the host
// registers its kind table before any runtime exists. This hook runs once per
// graph, when the runtime is known, which is where such a kind belongs.
//
// The hook receives [Runtime], not the assembly: the five methods a node kind
// reads. A host kind that needs more of the substrate - the file tree, the
// sandbox runner, the journal, the skill index - cannot get it here; such a kind
// is an engine kind, where deploy.EngineDeps.Runtime hands it the whole
// assembly.
func WithRuntimeKinds(build func(Runtime) ([]graph.NodeKind, error)) Option {
	return func(target *options) {
		if build != nil {
			target.builders = append(target.builders, build)
		}
	}
}

// WithKinds adds node kinds beside the built-in model, tool and condition.
func WithKinds(kinds ...graph.NodeKind) Option {
	return func(target *options) { target.kinds = append(target.kinds, kinds...) }
}

// WithModelDecoders supplies the provider extension decoders a model node's
// request.extensions resolves against.
func WithModelDecoders(decoders map[string]inference.ExtensionDecoder) Option {
	return func(target *options) { target.decoders = decoders }
}

// WithBlobStore lets the graph answer with media.
func WithBlobStore(store graph.BlobStore) Option {
	return func(target *options) { target.blobs = store }
}

// Open reads, validates and compiles a graph document for a runtime.
//
// The document is YAML; placeholders are expanded the same way a deployment
// document's are. name is what errors report, and body is where the bytes come
// from — [config.File], [config.Bytes], [config.FS] or [config.Func]. The
// registry is the built-in model, tool and condition kinds plus whatever the
// options add.
func Open(ctx context.Context, rt Runtime, name string, body config.Source, opts ...Option) (*graph.Graph, error) {
	document, err := load(ctx, name, body)
	if err != nil {
		return nil, err
	}
	var settings options
	for _, option := range opts {
		if option != nil {
			option(&settings)
		}
	}
	kinds, err := registryKinds(rt, settings)
	if err != nil {
		return nil, err
	}
	registry, err := graph.NewRegistry(kinds...)
	if err != nil {
		return nil, err
	}
	return graph.Build(document, registry, graph.WithBlobStore(settings.blobs))
}

// Validate checks a document without compiling it: a host reports a broken
// graph before it builds any resource.
func Validate(ctx context.Context, name string, body config.Source) error {
	_, err := load(ctx, name, body)
	return err
}

// load reads and parses one document, without a registry.
func load(ctx context.Context, name string, body config.Source) (graph.Document, error) {
	if body == nil {
		return graph.Document{}, errdefs.Validationf("graph: %s: no source", name)
	}
	raw, err := body.Body(ctx)
	if err != nil {
		return graph.Document{}, errdefs.Validationf("graph: %s: %w", name, err)
	}
	expanded, err := config.ExpandDocument(ctx, raw)
	if err != nil {
		return graph.Document{}, errdefs.Validationf("graph: %s: %w", name, err)
	}
	document, err := graph.Parse(expanded)
	if err != nil {
		return graph.Document{}, errdefs.Validationf("graph: %s: %w", name, err)
	}
	return document, nil
}

// registryKinds is the registry every graph starts with, plus the host's
// additions.
func registryKinds(rt Runtime, settings options) ([]graph.NodeKind, error) {
	built := []graph.NodeKind{
		Model(rt, WithExtensionDecoders(settings.decoders)),
		Tool(rt),
		Condition(),
	}
	built = append(built, settings.kinds...)
	for _, build := range settings.builders {
		added, err := build(rt)
		if err != nil {
			return nil, err
		}
		built = append(built, added...)
	}
	return built, nil
}
