package nodes

import (
	"context"
	"os"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// Option configures a graph opened for a runtime.
type Option func(*options)

type options struct {
	kinds    []graph.NodeKind
	decoders map[string]inference.ExtensionDecoder
	blobs    graph.BlobStore
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
// document's are. The registry is the built-in model, tool and condition kinds
// plus whatever the options add.
func Open(ctx context.Context, rt *runtime.Runtime, path string, opts ...Option) (*graph.Graph, error) {
	document, err := load(ctx, path)
	if err != nil {
		return nil, err
	}
	var settings options
	for _, option := range opts {
		if option != nil {
			option(&settings)
		}
	}
	registry, err := graph.NewRegistry(registryKinds(rt, settings)...)
	if err != nil {
		return nil, err
	}
	return graph.Build(document, registry, graph.WithBlobStore(settings.blobs))
}

// Validate checks a document without compiling it: a host reports a broken
// graph before it builds any resource.
func Validate(ctx context.Context, path string) error {
	_, err := load(ctx, path)
	return err
}

// load reads and parses one document, without a registry.
func load(ctx context.Context, path string) (graph.Document, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return graph.Document{}, errdefs.Validationf("graph: read %s: %w", path, err)
	}
	expanded, err := runtime.ExpandDocument(ctx, body)
	if err != nil {
		return graph.Document{}, errdefs.Validationf("graph: %s: %w", path, err)
	}
	document, err := graph.Parse(expanded)
	if err != nil {
		return graph.Document{}, errdefs.Validationf("graph: %s: %w", path, err)
	}
	return document, nil
}

// registryKinds is the registry every graph starts with, plus the host's
// additions.
func registryKinds(rt *runtime.Runtime, settings options) []graph.NodeKind {
	built := []graph.NodeKind{
		Model(rt, WithExtensionDecoders(settings.decoders)),
		Tool(rt),
		Condition(),
	}
	return append(built, settings.kinds...)
}
