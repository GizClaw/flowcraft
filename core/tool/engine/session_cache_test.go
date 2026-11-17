package engine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/engine"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// sizedDefinition builds a definition whose serialized size is size bytes,
// matching the engine's own accounting.
func sizedDefinition(name string, size int) message.ToolDefinition {
	const schema = `{"type":"object"}`
	pad := size - len(name) - len(schema) - 32
	if pad < 0 {
		panic("sizedDefinition: size too small for " + name)
	}
	return message.ToolDefinition{
		Name:        name,
		Description: strings.Repeat("x", pad),
		InputSchema: []byte(schema),
	}
}

// definitionTool wraps a sized definition into a working tool.
func definitionTool(def message.ToolDefinition) tool.Tool {
	return tool.TextTool(def, func(context.Context, string) (string, error) {
		return def.Name, nil
	})
}

// sizedLazyTool is a lazy descriptor whose placeholder and loaded definitions
// have known sizes, so a pool budget can fit the placeholder and reject the
// real schema.
func sizedLazyTool(name string, placeholderBytes, loadedBytes int, loads *atomic.Int32) tool.LazyTool {
	return tool.LazyTool{
		Name:        name,
		Placeholder: sizedDefinition(name, placeholderBytes),
		Load: func(context.Context) (tool.Tool, error) {
			loads.Add(1)
			return definitionTool(sizedDefinition(name, loadedBytes)), nil
		},
	}
}

// TestDefinitions_SnapshotTracksLoadedSize pins the epoch handshake between the
// catalog snapshot and a deferred load: once the real definition replaces the
// placeholder, the pool budget must be accounted against the definition now on
// the wire, even though this view already measured the placeholder. A snapshot
// published under a newer catalog epoch would keep the placeholder size and the
// pool would silently over-admit.
func TestDefinitions_SnapshotTracksLoadedSize(t *testing.T) {
	const (
		smallBytes = 300
		largeBytes = 900
		poolBytes  = 1000
	)
	var loads atomic.Int32
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{
			tools: []tool.Tool{
				sizedFuncTool("a", smallBytes),
				sizedFuncTool("b", smallBytes),
				sizedFuncTool("c", smallBytes),
			},
			lazyTools: []tool.LazyTool{sizedLazyTool("lazy", smallBytes, largeBytes, &loads)},
		}},
		engine.WithDynamic(engine.Policy{
			Default:   engine.ExposureDeferred,
			Discovery: engine.DiscoveryPolicy{MaxTools: 4, MaxBytes: poolBytes},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewCatalogView()

	// Fill the pool and measure the catalog while "lazy" still serves its
	// placeholder: this is the snapshot a stale publish would keep serving.
	if outcome := session.Discover("a", "b"); len(outcome.Evicted) != 0 {
		t.Fatalf("evicted %v before the pool was over budget", outcome.Evicted)
	}
	if err := session.EnsureLoaded(context.Background(), "lazy"); err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("loads = %d, want 1", got)
	}

	// 900 + 300 + 300 no longer fits 1000: recording a call on the loaded tool
	// evicts the two older entries. At the stale placeholder size the pool would
	// still fit and keep them.
	session.RecordCall(message.ToolCall{
		ID:        "c1",
		Name:      "lazy",
		Arguments: message.NewArguments([]byte(`{}`)),
	})

	// The pool state is what the stale size corrupted, and the next discovery is
	// where it shows: at 900 bytes nothing else fits beside the loaded tool, so it
	// is the one evicted. At the placeholder size the pool still had room and an
	// older 300-byte entry would have gone instead.
	outcome := session.Discover("c")
	if len(outcome.Evicted) != 1 || outcome.Evicted[0] != "lazy" {
		t.Fatalf("evicted = %v, want [lazy] dropped by its loaded definition size", outcome.Evicted)
	}
	names := definitionNames(session.Definitions())
	if !contains(names, "c") {
		t.Fatalf("Definitions = %v, want the newly discovered tool", names)
	}
}

// TestDefinitions_SnapshotSeesRegistrarAdd keeps the snapshot keyed to the
// catalog: a tool published through the Registrar must reach the next round
// instead of being frozen out by a cache from before the add.
func TestDefinitions_SnapshotSeesRegistrarAdd(t *testing.T) {
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{tools: []tool.Tool{funcTool("early", "1")}}},
		engine.WithDynamic(engine.Policy{Default: engine.ExposureAlways}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewCatalogView()
	if !contains(definitionNames(session.Definitions()), "early") {
		t.Fatalf("Definitions = %v, want early", definitionNames(session.Definitions()))
	}

	registry := assembly.Catalog().(*tool.Registry)
	if err := registry.Add(funcTool("late", "2")); err != nil {
		t.Fatalf("Registry.Add: %v", err)
	}
	if names := definitionNames(session.Definitions()); !contains(names, "late") {
		t.Fatalf("Definitions = %v, want the tool added through the Registrar", names)
	}
}

// TestDefinitions_ConcurrentCallsKeepBudget runs Definitions and RecordCall
// against one view. They share the catalog snapshot, so under -race this is
// where a stale publish would surface, and where a pool that grew past its cap
// would become visible to the model.
func TestDefinitions_ConcurrentCallsKeepBudget(t *testing.T) {
	const (
		toolBytes = 400
		poolTools = 2
		rounds    = 200
	)
	names := []string{"a", "b", "c", "d"}
	tools := make([]tool.Tool, 0, len(names))
	calls := make([]message.ToolCall, 0, len(names))
	for i, name := range names {
		tools = append(tools, sizedFuncTool(name, toolBytes))
		calls = append(calls, message.ToolCall{
			ID:        fmt.Sprintf("c%d", i),
			Name:      name,
			Arguments: message.NewArguments([]byte(`{}`)),
		})
	}
	assembly, err := engine.NewAssembly(
		[]tool.Source{source{tools: tools}},
		engine.WithDynamic(engine.Policy{
			Default:   engine.ExposureDeferred,
			Discovery: engine.DiscoveryPolicy{MaxTools: poolTools, MaxBytes: poolTools * toolBytes},
		}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewCatalogView()
	session.Discover(names...)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = session.Definitions()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			session.RecordCall(calls[i%len(calls)])
		}
	}()
	wg.Wait()

	pooled := 0
	for _, name := range definitionNames(session.Definitions()) {
		if contains(names, name) {
			pooled++
		}
	}
	if pooled > poolTools {
		t.Fatalf("%d pool entries visible, want at most %d", pooled, poolTools)
	}
}

// TestSession_LoadJoinsLazyErrors keeps the CatalogView contract: one failing
// source does not stop the others, and every failure stays reachable through
// errors.Is.
func TestSession_LoadJoinsLazyErrors(t *testing.T) {
	failA := errors.New("load a failed")
	failB := errors.New("load b failed")
	assembly, err := engine.NewAssembly(
		[]tool.Source{tooltest.LazySource(
			tooltest.LazyTool("a", func(context.Context) (tool.Tool, error) { return nil, failA }),
			tooltest.LazyTool("b", func(context.Context) (tool.Tool, error) { return nil, failB }),
		)},
		engine.WithDynamic(engine.Policy{Default: engine.ExposureDeferred}),
	)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	session := assembly.NewCatalogView()

	loadErr := session.Load(context.Background())
	if !errors.Is(loadErr, failA) || !errors.Is(loadErr, failB) {
		t.Fatalf("Load = %v, want both a and b failures joined", loadErr)
	}
	ensureErr := session.EnsureLoaded(context.Background(), "a", "b")
	if !errors.Is(ensureErr, failA) || !errors.Is(ensureErr, failB) {
		t.Fatalf("EnsureLoaded = %v, want both a and b failures joined", ensureErr)
	}
}
