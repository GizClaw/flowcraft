package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
)

const (
	captureKind = "resolver.test"
	captureImpl = "capture"
)

// captureEntry is one decoded settings record from a resolver capture
// resource, so tests can assert what the resolver-merged expansion
// pass materialized per build generation.
type captureEntry struct {
	root string
	name string
}

type captureRecorder struct {
	mu      sync.Mutex
	entries []captureEntry
}

func (r *captureRecorder) add(entry captureEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry)
}

func (r *captureRecorder) got() []captureEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]captureEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

// resolverCaptureFactory decodes the expanded settings of one
// "resolver.test" resource and records what it saw.
type resolverCaptureFactory struct {
	records *captureRecorder
}

func (f resolverCaptureFactory) Spec() resource.Spec {
	return resource.Spec{Kind: captureKind, Impl: captureImpl}
}

func (f resolverCaptureFactory) New(_ context.Context, in resource.Input) (any, error) {
	var settings struct {
		Root string `json:"root"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(in.Settings, &settings); err != nil {
		return nil, err
	}
	f.records.add(captureEntry{root: settings.Root, name: settings.Name})
	return &struct{}{}, nil
}

// ocwsResolver returns a resolver exposing a single "ocws" scheme that
// maps reference paths through values, standing in for a consumer's
// per-workspace configuration source.
func ocwsResolver(values map[string]string) *resource.ReferenceResolver {
	return resource.NewResolver(resource.SchemeFunc{
		SchemeName: "ocws",
		Fn: func(_ context.Context, ref resource.Reference) (any, error) {
			value, ok := values[ref.Path]
			if !ok {
				return "", errdefs.Validationf("ocws: unknown ref %q", ref.Path)
			}
			return value, nil
		},
	})
}

// resolverCaptureDoc returns a reload-shaped runtime doc whose agents
// use the append engine and whose extra "cap" resource carries the
// given raw settings JSON, expanded through the runtime's resolver.
func resolverCaptureDoc(t *testing.T, settings string) deploy.Document {
	t.Helper()
	doc := reloadDoc(t, `  bot:
    card: {name: Bot}
    engine: {kind: append-engine}
`, "")
	doc.Resources["cap"] = resource.Resource{
		Kind:     captureKind,
		Impl:     captureImpl,
		Settings: json.RawMessage(settings),
	}
	return doc
}

func newCaptureRegistry(t *testing.T, records *captureRecorder) *resource.Registry {
	t.Helper()
	reg := resource.NewRegistry()
	reg.MustRegister(resolverCaptureFactory{records: records})
	reg.MustRegister(event.NewFactory())
	reg.MustRegister(appendEngineFactory{})
	return reg
}

func assertEntries(t *testing.T, got, want []captureEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("captured settings = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("captured settings = %+v, want %+v", got, want)
		}
	}
}

func TestWithResolverValidation(t *testing.T) {
	builder := NewBuilder(resource.NewRegistry())
	if err := builder.WithResolver(nil); !errdefs.IsValidation(err) {
		t.Fatalf("nil resolver error = %v, want validation", err)
	}
	if err := builder.WithResolver(ocwsResolver(nil)); err != nil {
		t.Fatalf("first resolver: %v", err)
	}
	err := builder.WithResolver(ocwsResolver(nil))
	if err == nil || !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "already set") {
		t.Fatalf("duplicate resolver error = %v, want validation mentioning already set", err)
	}
}

func TestBuildExpandsCustomScheme(t *testing.T) {
	records := &captureRecorder{}
	builder := NewBuilder(newCaptureRegistry(t, records))
	if err := builder.WithResolver(ocwsResolver(map[string]string{
		"root": "/srv/ocws", "name": "alpha",
	})); err != nil {
		t.Fatalf("WithResolver: %v", err)
	}
	app, err := builder.Build(context.Background(),
		resolverCaptureDoc(t, `{"root": "${ocws:root}", "name": "${ocws:name}"}`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	assertEntries(t, records.got(),
		[]captureEntry{{root: "/srv/ocws", name: "alpha"}})
	if err := builder.WithResolver(ocwsResolver(nil)); !errors.Is(err, ErrBuilderUsed) {
		t.Fatalf("WithResolver after Build error = %v, want ErrBuilderUsed", err)
	}
}

func TestBuildRejectsUnknownSchemeWithoutResolver(t *testing.T) {
	records := &captureRecorder{}
	_, err := NewBuilder(newCaptureRegistry(t, records)).Build(
		context.Background(), resolverCaptureDoc(t, `{"root": "${ocws:root}"}`))
	if err == nil || !strings.Contains(err.Error(), `scheme "ocws" is not enabled`) {
		t.Fatalf("Build error = %v, want unresolved custom scheme error", err)
	}
}

func TestWithResolverKeepsBuiltinSchemes(t *testing.T) {
	t.Setenv("RESOLVER_TEST_ENV", "/from-env")
	records := &captureRecorder{}
	builder := NewBuilder(newCaptureRegistry(t, records))
	if err := builder.WithResolver(ocwsResolver(map[string]string{
		"name": "cfg",
	})); err != nil {
		t.Fatalf("WithResolver: %v", err)
	}
	app, err := builder.Build(context.Background(), resolverCaptureDoc(t,
		`{"root": "${env:RESOLVER_TEST_ENV}", "name": "${ocws:name}"}`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	assertEntries(t, records.got(),
		[]captureEntry{{root: "/from-env", name: "cfg"}})
}

func TestReloadKeepsCustomResolver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	records := &captureRecorder{}
	reg := newCaptureRegistry(t, records)
	values := map[string]string{"first": "one", "second": "two"}
	builder := NewBuilder(reg)
	if err := builder.WithResolver(ocwsResolver(values)); err != nil {
		t.Fatalf("WithResolver: %v", err)
	}
	app, err := builder.Build(ctx,
		resolverCaptureDoc(t, `{"root": "${ocws:first}", "name": "${ocws:second}"}`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	reloaded, err := app.Reload(ctx,
		resolverCaptureDoc(t, `{"root": "${ocws:second}", "name": "${ocws:first}"}`))
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if reloaded == nil {
		t.Fatal("Reload returned a nil result")
	}
	assertEntries(t, records.got(), []captureEntry{
		{root: "one", name: "two"},
		{root: "two", name: "one"},
	})
}

func TestResolverIsolationAcrossBuilds(t *testing.T) {
	doc := resolverCaptureDoc(t, `{"root": "${ocws:root}"}`)
	build := func(values map[string]string) (*Runtime, []captureEntry, error) {
		records := &captureRecorder{}
		builder := NewBuilder(newCaptureRegistry(t, records))
		if err := builder.WithResolver(ocwsResolver(values)); err != nil {
			return nil, nil, err
		}
		app, err := builder.Build(context.Background(), doc)
		if err != nil {
			return nil, nil, err
		}
		return app, records.got(), nil
	}
	var wg sync.WaitGroup
	apps := make([]*Runtime, 2)
	entries := make([][]captureEntry, 2)
	errs := make([]error, 2)
	for i := range apps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i == 0 {
				apps[i], entries[i], errs[i] = build(
					map[string]string{"root": "workspace-a"})
				return
			}
			apps[i], entries[i], errs[i] = build(
				map[string]string{"root": "workspace-b"})
		}()
	}
	wg.Wait()
	for i := range apps {
		if errs[i] != nil {
			t.Fatalf("build %d: %v", i, errs[i])
		}
		app := apps[i]
		t.Cleanup(func() { _ = app.Close() })
	}
	assertEntries(t, entries[0], []captureEntry{{root: "workspace-a"}})
	assertEntries(t, entries[1], []captureEntry{{root: "workspace-b"}})
}
