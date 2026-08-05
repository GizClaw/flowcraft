package component

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

type fakeDeriver struct{}

func (fakeDeriver) Derive(context.Context, Artifact) ([]Artifact, error) { return nil, nil }

type fakeIndexer struct{}

func (fakeIndexer) Rebuild(context.Context, ProjectionRequest) error { return nil }

type fakeSearcher struct{}

func (fakeSearcher) Search(context.Context, SearchRequest) ([]Candidate, error) { return nil, nil }

type fakePacker struct{}

func (fakePacker) Pack(context.Context, []sdkmemory.ContextItem, sdkmemory.Budget) (sdkmemory.ContextResult, error) {
	return sdkmemory.ContextResult{}, nil
}

func TestRegistryKeepsFourIndependentSlots(t *testing.T) {
	registry := NewRegistry()
	const name = "shared.v1"
	if err := registry.RegisterDeriver(name, func(Spec) (Deriver, error) { return fakeDeriver{}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterIndexer(name, func(Spec) (Indexer, error) { return fakeIndexer{}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterSearcher(name, func(Spec) (Searcher, error) { return fakeSearcher{}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterPacker(name, func(Spec) (Packer, error) { return fakePacker{}, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveDeriver(Spec{Name: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveIndexer(Spec{Name: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveSearcher(Spec{Name: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolvePacker(Spec{Name: name}); err != nil {
		t.Fatal(err)
	}

	err := registry.RegisterDeriver(name, func(Spec) (Deriver, error) { return fakeDeriver{}, nil })
	if !errors.Is(err, ErrFactoryConflict) {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestRegistryValidatesNamesAndPreservesFactoryCause(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"", " leading", "bad/name", "trailing "} {
		if err := registry.RegisterDeriver(name, func(Spec) (Deriver, error) { return fakeDeriver{}, nil }); err == nil {
			t.Fatalf("RegisterDeriver(%q) error = nil", name)
		}
	}
	cause := errors.New("factory failed")
	if err := registry.RegisterDeriver("broken", func(Spec) (Deriver, error) { return nil, cause }); err != nil {
		t.Fatal(err)
	}
	_, err := registry.ResolveDeriver(Spec{Name: "broken"})
	if !errors.Is(err, cause) {
		t.Fatalf("resolve error %v does not preserve cause", err)
	}
	if _, err := registry.ResolveDeriver(Spec{Name: "missing"}); !errors.Is(err, ErrFactoryNotFound) {
		t.Fatalf("missing error = %v", err)
	}
}

func TestNilRegistryReturnsErrors(t *testing.T) {
	var registry *Registry
	if err := registry.RegisterDeriver("deriver", func(Spec) (Deriver, error) {
		return fakeDeriver{}, nil
	}); err == nil {
		t.Fatal("RegisterDeriver on nil registry error = nil")
	}
	if _, err := registry.ResolveDeriver(Spec{Name: "deriver"}); err == nil {
		t.Fatal("ResolveDeriver on nil registry error = nil")
	}
}

type typedTestConfig struct {
	Values []string `json:"values"`
}

func TestRegistryClonesTypedFactoryConfig(t *testing.T) {
	registry := NewRegistry()
	var captured typedTestConfig
	if err := RegisterTypedDeriver(
		registry,
		"configured",
		"1.0.0",
		Ports{Inputs: []ArtifactKind{"source"}, Outputs: []ArtifactKind{"derived"}},
		func(config typedTestConfig) (Deriver, error) {
			captured = config
			config.Values[0] = "factory mutation"
			return fakeDeriver{}, nil
		},
	); err != nil {
		t.Fatal(err)
	}
	config := typedTestConfig{Values: []string{"original"}}
	if _, _, err := registry.ResolveTypedDeriver(NewDeriverSpec("configured", config)); err != nil {
		t.Fatal(err)
	}
	if config.Values[0] != "original" {
		t.Fatalf("factory mutated caller config: %#v", config)
	}
	config.Values[0] = "caller mutation"
	if captured.Values[0] != "factory mutation" {
		t.Fatal("captured config aliases caller config")
	}
	if _, _, err := registry.ResolveTypedDeriver(NewDeriverSpec("configured", struct{ Wrong bool }{})); err == nil {
		t.Fatal("accepted mismatched typed config")
	}
}

func TestTypedFactoryRejectsUntypedConfig(t *testing.T) {
	registry := NewRegistry()
	err := RegisterTypedDeriver(
		registry,
		"untyped",
		"1.0.0",
		Ports{Inputs: []ArtifactKind{"source"}, Outputs: []ArtifactKind{"derived"}},
		func(any) (Deriver, error) { return fakeDeriver{}, nil },
	)
	if err == nil {
		t.Fatal("accepted any as a typed factory config")
	}
}

func TestRegistryConcurrentRegisterAndResolve(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterDeriver("base", func(Spec) (Deriver, error) { return fakeDeriver{}, nil }); err != nil {
		t.Fatal(err)
	}
	const count = 64
	errs := make(chan error, count*2)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(2)
		go func(index int) {
			defer wait.Done()
			name := fmt.Sprintf("deriver-%d", index)
			errs <- registry.RegisterDeriver(name, func(Spec) (Deriver, error) { return fakeDeriver{}, nil })
		}(index)
		go func() {
			defer wait.Done()
			_, err := registry.ResolveDeriver(Spec{Name: "base"})
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < count; index++ {
		if _, err := registry.ResolveDeriver(Spec{Name: fmt.Sprintf("deriver-%d", index)}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArtifactValidateAndCloneOwnership(t *testing.T) {
	artifact := Artifact{
		Kind:    "chunk",
		ID:      "stable-id",
		Content: sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: "original"}}},
		Sources: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceDocument, ID: "doc"}},
		Metadata: sdkmemory.Metadata{
			"key": "original",
		},
	}
	if err := artifact.Validate(); err != nil {
		t.Fatal(err)
	}
	cloned := artifact.Clone()
	cloned.Content.Parts[0] = sdkmessage.TextPart{Text: "changed"}
	cloned.Sources[0].ID = "changed"
	cloned.Metadata["key"] = "changed"
	if artifact.Content.Text() != "original" || artifact.Sources[0].ID != "doc" || artifact.Metadata["key"] != "original" {
		t.Fatalf("clone aliases artifact: %#v", artifact)
	}

	bad := artifact
	bad.Sources = nil
	if err := bad.Validate(); err == nil {
		t.Fatal("artifact without provenance was accepted")
	}
}

func TestCandidateRetainsLaneNativeScore(t *testing.T) {
	candidate := Candidate{
		ID:     "candidate",
		Lane:   "bm25",
		Name:   "messages",
		Score:  -17.5,
		Source: sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "message"},
	}
	if err := candidate.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidate.Score, -17.5) {
		t.Fatalf("score changed to %v", candidate.Score)
	}
}
