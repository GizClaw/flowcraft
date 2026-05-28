package assembly

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/vessel"
)

// BackendDeps is the shared resource bundle handed to backend factories.
type BackendDeps struct {
	Manifest  Manifest
	Workspace WorkspaceHandle
}

// WorkspaceBackend builds the shared workspace and per-run session store.
type WorkspaceBackend interface {
	ValidateWorkspace(WorkspaceSpec) error
	BuildWorkspace(context.Context, WorkspaceSpec) (WorkspaceResource, error)
}

type WorkspaceResource struct {
	Workspace    WorkspaceHandle
	SessionStore vessel.SessionStore
	Closers      []func(context.Context) error
}

// RecallBackend builds a recall.Memory implementation.
type RecallBackend interface {
	ValidateRecall(RecallSpec) error
	BuildRecall(context.Context, RecallSpec, BackendDeps) (RecallResource, error)
}

type RecallResource struct {
	Memory          RecallHandle
	ScopeEnumerator recall.ScopeEnumerator
	Closers         []func(context.Context) error
}

// KnowledgeBackend builds a memory/knowledge service implementation.
type KnowledgeBackend interface {
	ValidateKnowledge(KnowledgeSpec) error
	BuildKnowledge(context.Context, KnowledgeSpec, BackendDeps) (KnowledgeResource, error)
}

type KnowledgeResource struct {
	Service KnowledgeHandle
	Closers []func(context.Context) error
}
