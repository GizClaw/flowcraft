package plugin

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Source is one plugin's tool contributor: a [*wireSource] in production,
// a fake in tests. A source may also implement [Readiness], which is
// what lets an enable wait for the plugin instead of only spawning it.
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

// Readiness is optionally implemented by sources that can report when
// they are up. An enable waits on it ([Host.SetEnabled]), so a plugin that
// cannot come up is reported by the call that asked for it; a source that
// does not implement it is enabled without a wait, which is how every
// source was treated before the seam existed.
//
// WaitReady returns nil once every server of the plugin is serving, and an
// error when the bound (or ctx) elapses first. A Validation-class error is
// the terminal case — the source will not come up on its own — and is what
// makes an enable roll back; any other error means "not yet", and the
// enable stands with the source still reconnecting.
type Readiness interface {
	WaitReady(ctx context.Context, timeout time.Duration) error
}

// ToolSet aggregates every enabled plugin's tools and fans additions
// and removals out to every attached registry (one per runtime
// generation). It implements tool.Source, tool.RegistryAttacher and
// tool.RegistryDetacher, so it can be injected as the craft.plugins
// external: a generation attaches when it builds and detaches when it
// closes.
type ToolSet struct {
	mu         sync.Mutex
	children   map[string]*childEntry
	downstream map[tool.Registrar]struct{}
	tools      map[string]ownerTool
	// drainTimeout bounds how long Close waits for one plugin's
	// in-flight calls; the host sets it from its own drain timeout (see
	// [HostOptions.DrainTimeout]). Zero means [DefaultDrainTimeout].
	drainTimeout time.Duration
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
	return newToolSet(0)
}

// newToolSet returns an empty aggregator whose Close is bounded by
// drainTimeout, the host's own bound: the aggregator drains the same
// processes the host stops, so the two waits have to be the same one.
// Zero means [DefaultDrainTimeout].
func newToolSet(drainTimeout time.Duration) *ToolSet {
	return &ToolSet{
		children:     make(map[string]*childEntry),
		downstream:   make(map[tool.Registrar]struct{}),
		tools:        make(map[string]ownerTool),
		drainTimeout: drainTimeout,
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

// Detach implements tool.RegistryDetacher: the generation that built
// the registrar has closed, so stop fanning plugin publications out to
// it. Without this, the retired generation stays reachable from this
// process-wide set for as long as the host lives.
func (t *ToolSet) Detach(registrar tool.Registrar) {
	if t == nil || registrar == nil {
		return
	}
	t.prune(registrar)
}

// Attached reports how many live downstream registries are attached.
// Closed registries detach, so the count tracks live runtime
// generations.
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

// RemovePlugin closes the plugin to new calls, removes its tools from
// every registry, waits for the calls in flight bounded by ctx, then
// closes the source.
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

	// The drain's cutoff is here, not somewhere inside the fan-out
	// below: a tool a caller resolved a moment ago is refused from now
	// on, instead of being admitted into a wait that is about to stop
	// accounting for it. The wait then covers exactly the calls this
	// drain was decided with.
	entry.flight.stop()
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
//
// Each plugin's drain is bounded by the host's drain timeout: a call
// that does not finish in time is reported, and the source is closed
// anyway — an unbounded close would let one hung call keep the whole
// process from shutting down.
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
		ctx, cancel := context.WithTimeout(context.Background(), t.drainBound())
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

// drainBound is how long one plugin's drain may take: the host's drain
// timeout, or the package default for a ToolSet nobody wired.
func (t *ToolSet) drainBound() time.Duration {
	if t.drainTimeout > 0 {
		return t.drainTimeout
	}
	return DefaultDrainTimeout
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

// prune drops a registry that is no longer live: its close was
// reported through Detach, or it rejected a publication (typically a
// closed registry from a retired generation).
func (t *ToolSet) prune(registrar tool.Registrar) {
	t.mu.Lock()
	delete(t.downstream, registrar)
	t.mu.Unlock()
}

// flight counts one plugin's in-flight tool executions and closes itself
// to new ones when the plugin's drain begins.
//
// The closing half is what makes the count mean anything: a call that
// begins after the drain started would extend a wait that has already
// finished, and the process would be closed under it. Counting with a
// wait group alone cannot express that — its Add races its Wait — so the
// count and the closing flag live under one lock, and a call arriving
// once the drain began is refused instead of counted.
type flight struct {
	mu      sync.Mutex
	closing bool
	active  int
	// drained is closed when the last in-flight call ends; a drain that
	// begins with nothing in flight gets a closed one instead.
	drained chan struct{}
}

// begin counts one call, or refuses it because the plugin is draining.
// A refused call never reaches the process: it is answered by the error,
// not by a dead server.
func (f *flight) begin() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closing {
		return errdefs.Conflictf(
			"plugin tools: the plugin is stopping; retry its next generation")
	}
	f.active++
	return nil
}

// end drops one counted call, waking a drain when it was the last.
func (f *flight) end() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active--
	if f.active == 0 && f.drained != nil {
		close(f.drained)
		f.drained = nil
	}
}

// stop closes the flight to new calls without waiting for the ones in
// flight. It is the drain's cutoff: the calls that are counted by the
// time it ran are the ones a drain is a wait on.
func (f *flight) stop() {
	f.mu.Lock()
	f.closing = true
	f.mu.Unlock()
}

// wait closes the flight to new calls and waits for the ones in flight,
// bounded by ctx. A caller that gives up waiting keeps the refusal:
// whatever it is about to do to the process, no new call may join the
// set it stopped accounting for.
func (f *flight) wait(ctx context.Context) error {
	f.stop()
	f.mu.Lock()
	if f.active == 0 {
		f.mu.Unlock()
		return nil
	}
	if f.drained == nil {
		f.drained = make(chan struct{})
	}
	drained := f.drained
	f.mu.Unlock()
	select {
	case <-drained:
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
	if err := t.flight.begin(); err != nil {
		return message.Content{}, err
	}
	defer t.flight.end()
	return t.Tool.Execute(ctx, arguments)
}

var (
	_ tool.Source           = (*ToolSet)(nil)
	_ tool.RegistryAttacher = (*ToolSet)(nil)
	_ tool.RegistryDetacher = (*ToolSet)(nil)
)
