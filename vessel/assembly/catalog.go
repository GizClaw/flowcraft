package assembly

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/memory/knowledge"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// ToolFactory creates an SDK tool after Build has assembled shared resources.
type ToolFactory func(context.Context, ToolDeps) (tool.Tool, error)

// ToolDeps exposes assembled resources to custom tool factories.
type ToolDeps struct {
	Manifest  Manifest
	Workspace WorkspaceHandle
	Recall    RecallHandle
	Knowledge KnowledgeHandle
}

// Catalog is the user-extensible registry that maps declarative manifest IDs
// to Go factories. It is safe to reuse across concurrent Build calls.
type Catalog struct {
	mu sync.RWMutex

	engines   map[string]vessel.EngineFactory
	tools     map[string]ToolFactory
	embedders map[string]knowledge.Embedder
	resolver  llm.LLMResolver

	workspaces map[string]WorkspaceBackend
	recalls    map[string]RecallBackend
	knowledges map[string]KnowledgeBackend
}

func NewCatalog() *Catalog {
	return &Catalog{
		engines:   make(map[string]vessel.EngineFactory),
		tools:     make(map[string]ToolFactory),
		embedders: make(map[string]knowledge.Embedder),
		workspaces: map[string]WorkspaceBackend{
			WorkspaceBackendMemory:     MemoryWorkspaceBackend(),
			WorkspaceBackendFilesystem: FilesystemWorkspaceBackend(),
		},
		recalls: map[string]RecallBackend{
			RecallBackendMemory:    MemoryRecallBackend(),
			RecallBackendWorkspace: WorkspaceRecallBackend(),
		},
		knowledges: map[string]KnowledgeBackend{
			KnowledgeBackendNone:      NoKnowledgeBackend(),
			KnowledgeBackendWorkspace: WorkspaceKnowledgeBackend(),
		},
	}
}

func (c *Catalog) RegisterEngine(kind string, f vessel.EngineFactory) error {
	if kind == "" {
		return errdefs.Validationf("vessel assembly: engine kind is required")
	}
	if f == nil {
		return errdefs.Validationf("vessel assembly: engine factory for %q is nil", kind)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.engines[kind] = f
	return nil
}

func (c *Catalog) RegisterTool(name string, f ToolFactory) error {
	if name == "" {
		return errdefs.Validationf("vessel assembly: tool name is required")
	}
	if f == nil {
		return errdefs.Validationf("vessel assembly: tool factory for %q is nil", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tools[name] = f
	return nil
}

func (c *Catalog) RegisterEmbedder(name string, embedder knowledge.Embedder) error {
	if name == "" {
		return errdefs.Validationf("vessel assembly: embedder name is required")
	}
	if embedder == nil {
		return errdefs.Validationf("vessel assembly: embedder %q is nil", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.embedders[name] = embedder
	return nil
}

func (c *Catalog) RegisterWorkspaceBackend(name string, backend WorkspaceBackend) error {
	if name == "" {
		return errdefs.Validationf("vessel assembly: workspace backend name is required")
	}
	if backend == nil {
		return errdefs.Validationf("vessel assembly: workspace backend %q is nil", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.workspaces[name] = backend
	return nil
}

func (c *Catalog) RegisterRecallBackend(name string, backend RecallBackend) error {
	if name == "" {
		return errdefs.Validationf("vessel assembly: recall backend name is required")
	}
	if backend == nil {
		return errdefs.Validationf("vessel assembly: recall backend %q is nil", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recalls[name] = backend
	return nil
}

func (c *Catalog) RegisterKnowledgeBackend(name string, backend KnowledgeBackend) error {
	if name == "" {
		return errdefs.Validationf("vessel assembly: knowledge backend name is required")
	}
	if backend == nil {
		return errdefs.Validationf("vessel assembly: knowledge backend %q is nil", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.knowledges[name] = backend
	return nil
}

func (c *Catalog) SetLLMResolver(resolver llm.LLMResolver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolver = resolver
}

func (c *Catalog) engineFactory(kind string) (vessel.EngineFactory, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	f, ok := c.engines[kind]
	return f, ok
}

func (c *Catalog) toolFactories() map[string]ToolFactory {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]ToolFactory, len(c.tools))
	for name, f := range c.tools {
		out[name] = f
	}
	return out
}

func (c *Catalog) llmResolver() llm.LLMResolver {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.resolver
}

func (c *Catalog) workspaceBackend(name string) (WorkspaceBackend, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	backend, ok := c.workspaces[name]
	return backend, ok
}

func (c *Catalog) recallBackend(name string) (RecallBackend, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	backend, ok := c.recalls[name]
	return backend, ok
}

func (c *Catalog) knowledgeBackend(name string) (KnowledgeBackend, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	backend, ok := c.knowledges[name]
	return backend, ok
}

func resolveWorkspaceBackend(name string, defaults Defaults, catalog *Catalog) (WorkspaceBackend, error) {
	if name == "" {
		if defaults.Workspace == nil {
			return nil, errdefs.Validationf("vessel assembly: default workspace backend is nil")
		}
		return defaults.Workspace, nil
	}
	if catalog == nil {
		catalog = NewCatalog()
	}
	if backend, ok := catalog.workspaceBackend(name); ok {
		return backend, nil
	}
	return nil, errdefs.Validationf("vessel assembly: unsupported workspace backend %q", name)
}

func resolveRecallBackend(name string, defaults Defaults, catalog *Catalog) (RecallBackend, error) {
	if name == "" {
		if defaults.Recall == nil {
			return nil, errdefs.Validationf("vessel assembly: default recall backend is nil")
		}
		return defaults.Recall, nil
	}
	if catalog == nil {
		catalog = NewCatalog()
	}
	if backend, ok := catalog.recallBackend(name); ok {
		return backend, nil
	}
	return nil, errdefs.Validationf("vessel assembly: unsupported recall backend %q", name)
}

func resolveKnowledgeBackend(name string, defaults Defaults, catalog *Catalog) (KnowledgeBackend, error) {
	if name == "" {
		if defaults.Knowledge == nil {
			return nil, errdefs.Validationf("vessel assembly: default knowledge backend is nil")
		}
		return defaults.Knowledge, nil
	}
	if catalog == nil {
		catalog = NewCatalog()
	}
	if backend, ok := catalog.knowledgeBackend(name); ok {
		return backend, nil
	}
	return nil, errdefs.Validationf("vessel assembly: unsupported knowledge backend %q", name)
}

// EngineFunc is a convenience helper for tests and simple manifests.
func EngineFunc(fn func(context.Context, engine.Run, engine.Host, *engine.Board) (*engine.Board, error)) engine.Engine {
	return engine.EngineFunc(fn)
}

func dispatchEngineFactory(c *Catalog) vessel.EngineFactory {
	return func(aspec spec.Agent, deps vessel.Deps) (engine.Engine, error) {
		kind := aspec.EngineKind
		if kind == "" {
			kind = "default"
		}
		f, ok := c.engineFactory(kind)
		if !ok {
			return nil, errdefs.Validationf("vessel assembly: no engine factory registered for %q", kind)
		}
		return f(aspec, deps)
	}
}
