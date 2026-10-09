package plugin

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Source is one plugin's tool contributor: a mcp.Source in production,
// a fake in tests.
type Source interface {
	Tools() []tool.Tool
	LazyTools() []tool.LazyTool
	Attach(tool.Registrar)
	Close() error
}

// Caller is optionally implemented by sources that support direct tool
// invocation; plugin graph nodes use it.
type Caller interface {
	CallTool(ctx context.Context, toolName string, args any) (json.RawMessage, error)
}

// ToolSet aggregates every enabled plugin's tools and fans additions
// and removals out to every attached registry (one per runtime
// generation). It implements tool.Source and tool.RegistryAttacher, so
// it can be injected as the craft.plugins external.
type ToolSet struct {
	mu         sync.Mutex
	children   map[string]*childEntry
	downstream map[tool.Registrar]struct{}
	tools      map[string]ownerTool
}

type childEntry struct {
	source Source
	flight *flight
	tools  map[string]struct{}
}

type ownerTool struct {
	owner string
	tool  tool.Tool
}

// NewToolSet returns an empty aggregator.
func NewToolSet() *ToolSet {
	return &ToolSet{
		children:   make(map[string]*childEntry),
		downstream: make(map[tool.Registrar]struct{}),
		tools:      make(map[string]ownerTool),
	}
}

// Tools implements tool.Source.
func (t *ToolSet) Tools() []tool.Tool {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	out := make([]tool.Tool, 0, len(t.tools))
	for _, entry := range t.tools {
		out = append(out, entry.tool)
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].Definition().Name < out[j].Definition().Name
	})
	return out
}

// LazyTools implements tool.Source.
func (t *ToolSet) LazyTools() []tool.LazyTool { return nil }

// Attach implements tool.RegistryAttacher: a runtime registry attaches
// here and receives every current plugin tool.
func (t *ToolSet) Attach(registrar tool.Registrar) {
	if t == nil || registrar == nil {
		return
	}
	t.mu.Lock()
	t.downstream[registrar] = struct{}{}
	current := make([]tool.Tool, 0, len(t.tools))
	for _, entry := range t.tools {
		current = append(current, entry.tool)
	}
	t.mu.Unlock()
	for _, candidate := range current {
		if err := registrar.Add(candidate); err != nil &&
			errdefs.IsNotAvailable(err) {
			t.prune(registrar)
		}
	}
}

// Attached reports how many live downstream registries are attached.
func (t *ToolSet) Attached() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.downstream)
}

// AddPlugin starts tracking one plugin source.
func (t *ToolSet) AddPlugin(id string, source Source) error {
	if t == nil || source == nil {
		return errdefs.Validationf("plugin tools: source is required")
	}
	t.mu.Lock()
	if _, duplicate := t.children[id]; duplicate {
		t.mu.Unlock()
		return errdefs.Conflictf("plugin tools: plugin %q already added", id)
	}
	entry := &childEntry{
		source: source,
		flight: &flight{},
		tools:  make(map[string]struct{}),
	}
	t.children[id] = entry
	t.mu.Unlock()
	source.Attach(&pluginRegistrar{set: t, plugin: id})
	return nil
}

// RemovePlugin removes the plugin's tools from every registry, waits
// for in-flight calls bounded by ctx, then closes the source.
func (t *ToolSet) RemovePlugin(ctx context.Context, id string) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	entry := t.children[id]
	if entry == nil {
		t.mu.Unlock()
		return nil
	}
	delete(t.children, id)
	names := make([]string, 0, len(entry.tools))
	for name := range entry.tools {
		names = append(names, name)
		delete(t.tools, name)
	}
	downstream := make([]tool.Registrar, 0, len(t.downstream))
	for registrar := range t.downstream {
		downstream = append(downstream, registrar)
	}
	t.mu.Unlock()

	for _, registrar := range downstream {
		for _, name := range names {
			registrar.Remove(name)
		}
	}
	if err := entry.flight.wait(ctx); err != nil {
		_ = entry.source.Close()
		return err
	}
	return entry.source.Close()
}

// Close removes every plugin and releases the sources.
func (t *ToolSet) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	ids := make([]string, 0, len(t.children))
	for id := range t.children {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	sort.Strings(ids)
	var errs []error
	for _, id := range ids {
		ctx, cancel := context.WithCancel(context.Background())
		if err := t.RemovePlugin(ctx, id); err != nil {
			errs = append(errs, err)
		}
		cancel()
	}
	t.mu.Lock()
	t.downstream = make(map[tool.Registrar]struct{})
	t.mu.Unlock()
	if len(errs) == 0 {
		return nil
	}
	return errdefs.Internal(errs[0])
}

// pluginRegistrar scopes child-source publications to one plugin.
type pluginRegistrar struct {
	set    *ToolSet
	plugin string
}

func (r *pluginRegistrar) Add(candidate tool.Tool) error {
	return r.set.addFromPlugin(r.plugin, candidate)
}

func (r *pluginRegistrar) Remove(name string) {
	r.set.removeFromPlugin(r.plugin, name)
}

func (t *ToolSet) addFromPlugin(id string, candidate tool.Tool) error {
	if candidate == nil {
		return errdefs.Validationf("plugin %s: nil tool", id)
	}
	name := candidate.Definition().Name
	t.mu.Lock()
	entry := t.children[id]
	if entry == nil {
		t.mu.Unlock()
		return nil
	}
	if _, duplicate := t.tools[name]; duplicate {
		t.mu.Unlock()
		return errdefs.Conflictf("plugin tools: duplicate tool %q", name)
	}
	entry.tools[name] = struct{}{}
	wrapped := trackedTool{Tool: candidate, flight: entry.flight}
	t.tools[name] = ownerTool{owner: id, tool: wrapped}
	downstream := make([]tool.Registrar, 0, len(t.downstream))
	for registrar := range t.downstream {
		downstream = append(downstream, registrar)
	}
	t.mu.Unlock()

	for _, registrar := range downstream {
		if err := registrar.Add(wrapped); err != nil &&
			errdefs.IsNotAvailable(err) {
			t.prune(registrar)
		}
	}
	return nil
}

func (t *ToolSet) removeFromPlugin(id, name string) {
	t.mu.Lock()
	entry := t.children[id]
	if entry == nil {
		t.mu.Unlock()
		return
	}
	if _, ok := entry.tools[name]; !ok {
		t.mu.Unlock()
		return
	}
	delete(entry.tools, name)
	delete(t.tools, name)
	downstream := make([]tool.Registrar, 0, len(t.downstream))
	for registrar := range t.downstream {
		downstream = append(downstream, registrar)
	}
	t.mu.Unlock()
	for _, registrar := range downstream {
		registrar.Remove(name)
	}
}

// prune drops a registry that rejected a publication (typically a
// closed registry from a retired generation).
func (t *ToolSet) prune(registrar tool.Registrar) {
	t.mu.Lock()
	delete(t.downstream, registrar)
	t.mu.Unlock()
}

// flight counts in-flight tool executions for one plugin.
type flight struct {
	wg sync.WaitGroup
}

func (f *flight) begin() { f.wg.Add(1) }
func (f *flight) end()   { f.wg.Done() }

func (f *flight) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errdefs.FromContext(ctx.Err())
	}
}

// trackedTool counts one plugin tool execution so a disable/update can
// drain before closing the process.
type trackedTool struct {
	tool.Tool
	flight *flight
}

func (t trackedTool) Execute(
	ctx context.Context,
	arguments string,
) (message.Content, error) {
	t.flight.begin()
	defer t.flight.end()
	return t.Tool.Execute(ctx, arguments)
}
