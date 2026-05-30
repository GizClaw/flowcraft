package assembly

import (
	"context"
	"fmt"

	knowledgefs "github.com/GizClaw/flowcraft/memory/knowledge/backend/fs"
	knowledgefactory "github.com/GizClaw/flowcraft/memory/knowledge/factory"
	retrievalmem "github.com/GizClaw/flowcraft/memory/retrieval/memory"
)

type knowledgeBundle struct {
	service KnowledgeHandle
	closers []func(context.Context) error
}

func buildKnowledge(ctx context.Context, spec *KnowledgeSpec, ws WorkspaceHandle, defaults Defaults, catalog *Catalog, manifest Manifest) (knowledgeBundle, error) {
	if spec == nil {
		return knowledgeBundle{}, nil
	}
	backend, err := resolveKnowledgeBackend(spec.Backend, defaults, catalog)
	if err != nil {
		return knowledgeBundle{}, err
	}
	res, err := backend.BuildKnowledge(ctx, *spec, BackendDeps{Manifest: manifest, Workspace: ws})
	if err != nil {
		return knowledgeBundle{}, err
	}
	return knowledgeBundle{service: res.Service, closers: res.Closers}, nil
}

type noKnowledgeBackend struct{}

func NoKnowledgeBackend() KnowledgeBackend { return noKnowledgeBackend{} }

func (noKnowledgeBackend) ValidateKnowledge(KnowledgeSpec) error { return nil }

func (noKnowledgeBackend) BuildKnowledge(context.Context, KnowledgeSpec, BackendDeps) (KnowledgeResource, error) {
	return KnowledgeResource{}, nil
}

type workspaceKnowledgeBackend struct{}

func WorkspaceKnowledgeBackend() KnowledgeBackend { return workspaceKnowledgeBackend{} }

func (workspaceKnowledgeBackend) ValidateKnowledge(KnowledgeSpec) error { return nil }

func (workspaceKnowledgeBackend) BuildKnowledge(_ context.Context, spec KnowledgeSpec, deps BackendDeps) (KnowledgeResource, error) {
	if deps.Workspace == nil {
		return KnowledgeResource{}, fmt.Errorf("vessel assembly: knowledge workspace backend requires workspace")
	}
	idx := retrievalmem.New()
	docs := knowledgefs.NewDocumentRepo(deps.Workspace, defaultString(spec.Prefix, "knowledge"))
	svc := knowledgefactory.NewRetrieval(docs, idx)
	return KnowledgeResource{
		Service: svc,
		Closers: []func(context.Context) error{
			func(context.Context) error { return idx.Close() },
		},
	}, nil
}
