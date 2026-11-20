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
	// configOptions are the host's placeholder resolvers: the ones it registered
	// for the deployment's own documents, handed here so a node config names a
	// scheme rather than carrying a value.
	configOptions []config.Option
}

// WithConfigOptions hands the host's placeholder resolvers to the loader that
// reads the graph document.
//
// A graph document is expanded by the same code a runtime layer is, so a node
// config may name a reference the host knows how to resolve - a credential, a
// row in a table, a path inside a workspace. The scheme itself is the host's:
// only ${env:...} is built in, and an unregistered scheme is an error that lists
// what is registered, not a literal handed to a node.
//
// A document that does not need a scheme costs nothing for its absence: the
// resolvers are looked up only when a placeholder names one.
func WithConfigOptions(opts ...config.Option) Option {
	return func(target *options) { target.configOptions = append(target.configOptions, opts...) }
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
// document's are, through the resolvers [WithConfigOptions] supplies. name is
// what errors report, and body is where the bytes come
// from — [config.File], [config.Bytes], [config.FS] or [config.Func]. The
// registry is the built-in model, tool and condition kinds plus whatever the
// options add.
func Open(ctx context.Context, rt Runtime, name string, body config.Source, opts ...Option) (*graph.Graph, error) {
	settings := newOptions(opts)
	document, err := load(ctx, name, body, settings)
	if err != nil {
		return nil, err
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
//
// It takes the same options [Open] does, because a document that names a
// placeholder has to be expanded before it can be checked: a scheme the host did
// not register is a load error here, not something the first build discovers.
func Validate(ctx context.Context, name string, body config.Source, opts ...Option) error {
	_, err := load(ctx, name, body, newOptions(opts))
	return err
}

// RequiredPlaceholders reports what a graph document references, without
// resolving any of it.
//
// Expansion fails at the first unset reference, so a host that wants to print a
// startup checklist needs the whole list before a load: this is the graph
// document's counterpart of [deploy.Loader.RequiredPlaceholdersFile], over the
// same scanner. A reference that names a scheme other than env is reported as
// "scheme:args", which is what the host registered the scheme under.
func RequiredPlaceholders(ctx context.Context, name string, body config.Source) (config.Requirement, error) {
	raw, err := readBody(ctx, name, body)
	if err != nil {
		return config.Requirement{}, err
	}
	requirement, err := config.RequiredPlaceholdersDocument(raw, name)
	if err != nil {
		return config.Requirement{}, errdefs.Validationf("graph: %w", err)
	}
	return requirement, nil
}

// newOptions applies the host's options in order, so a later one wins.
func newOptions(opts []Option) options {
	var settings options
	for _, option := range opts {
		if option != nil {
			option(&settings)
		}
	}
	return settings
}

// expandOptions is how the document is expanded: the label names what was read,
// and the host's resolvers follow it, so a host that wants its own label wins.
func (o options) expandOptions() []config.Option {
	return append([]config.Option{config.WithLabel("document")}, o.configOptions...)
}

// readBody reads the document's bytes, naming it the way every error here does.
func readBody(ctx context.Context, name string, body config.Source) ([]byte, error) {
	if body == nil {
		return nil, errdefs.Validationf("graph: %s: no source", name)
	}
	raw, err := body.Body(ctx)
	if err != nil {
		return nil, errdefs.Validationf("graph: %s: %w", name, err)
	}
	return raw, nil
}

// load reads and parses one document, without a registry.
func load(ctx context.Context, name string, body config.Source, settings options) (graph.Document, error) {
	raw, err := readBody(ctx, name, body)
	if err != nil {
		return graph.Document{}, err
	}
	expanded, err := config.ExpandDocument(ctx, raw, settings.expandOptions()...)
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
