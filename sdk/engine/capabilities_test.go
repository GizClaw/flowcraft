package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
)

type describingEngine struct {
	caps engine.Capabilities
}

func (describingEngine) Execute(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
	return b, nil
}

func (e describingEngine) Capabilities() engine.Capabilities { return e.caps }

func TestCapabilitiesOf_DefaultsToZeroForPlainEngine(t *testing.T) {
	plain := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		return b, nil
	})
	got := engine.CapabilitiesOf(plain)
	if got.SupportsResume || got.EmitsUserPrompt || got.EmitsCheckpoint || len(got.RequiredDepNames) != 0 {
		t.Fatalf("plain engine must report zero Capabilities; got %+v", got)
	}
}

func TestCapabilitiesOf_ReadsDescriber(t *testing.T) {
	want := engine.Capabilities{
		SupportsResume:   true,
		EmitsUserPrompt:  true,
		EmitsCheckpoint:  true,
		RequiredDepNames: []string{"llm.resolver", "tool.registry"},
	}
	got := engine.CapabilitiesOf(describingEngine{caps: want})
	if got.SupportsResume != true ||
		got.EmitsUserPrompt != true ||
		got.EmitsCheckpoint != true ||
		len(got.RequiredDepNames) != 2 ||
		got.RequiredDepNames[0] != "llm.resolver" ||
		got.RequiredDepNames[1] != "tool.registry" {
		t.Fatalf("CapabilitiesOf = %+v, want %+v", got, want)
	}
}

type suggestingEngine struct {
	describingEngine
	calls int
	err   error
}

func (s *suggestingEngine) SuggestCheckpoint() error {
	s.calls++
	return s.err
}

func TestSuggestCheckpoint_NoOpForEnginesWithoutInterface(t *testing.T) {
	plain := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		return b, nil
	})
	if err := engine.SuggestCheckpoint(plain); err != nil {
		t.Fatalf("SuggestCheckpoint on a plain engine must be a nil-returning no-op; got %v", err)
	}
}

func TestSuggestCheckpoint_DelegatesToImplementation(t *testing.T) {
	se := &suggestingEngine{err: errors.New("snapshot failed")}
	got := engine.SuggestCheckpoint(se)
	if se.calls != 1 {
		t.Fatalf("SuggestCheckpoint must call the engine once; got %d", se.calls)
	}
	if got == nil || got.Error() != "snapshot failed" {
		t.Fatalf("SuggestCheckpoint must surface the engine error; got %v", got)
	}
}

// TestWithCapabilities_AddsDescriberToPlainEngine pins the basic
// adapter contract: an engine that does not implement Describer
// can be wrapped to advertise capabilities to CapabilitiesOf
// without changing its Execute behaviour.
func TestWithCapabilities_AddsDescriberToPlainEngine(t *testing.T) {
	executed := false
	plain := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		executed = true
		return b, nil
	})

	caps := engine.Capabilities{
		EmitsCheckpoint:  true,
		RequiredDepNames: []string{"llm.client"},
	}
	wrapped := engine.WithCapabilities(plain, caps)

	got := engine.CapabilitiesOf(wrapped)
	if !got.EmitsCheckpoint {
		t.Fatalf("CapabilitiesOf(wrapped).EmitsCheckpoint = false, want true")
	}
	if len(got.RequiredDepNames) != 1 || got.RequiredDepNames[0] != "llm.client" {
		t.Fatalf("CapabilitiesOf(wrapped).RequiredDepNames = %v, want [llm.client]", got.RequiredDepNames)
	}

	board := engine.NewBoard()
	if _, err := wrapped.Execute(context.Background(), engine.Run{ID: "r"}, engine.NoopHost{}, board); err != nil {
		t.Fatalf("wrapped Execute returned %v", err)
	}
	if !executed {
		t.Fatalf("WithCapabilities must forward Execute to the underlying engine")
	}
}

// TestWithCapabilities_NilEngineReturnsNil ensures the helper is a
// no-op for nil — callers wrapping engines from a registry can
// pass the lookup result through without nil-checking.
func TestWithCapabilities_NilEngineReturnsNil(t *testing.T) {
	if got := engine.WithCapabilities(nil, engine.Capabilities{}); got != nil {
		t.Fatalf("WithCapabilities(nil, ...) = %v, want nil", got)
	}
}

// TestWithCapabilities_PreservesResumer asserts that wrapping a
// Resumer-implementing engine does NOT hide the Resumer interface
// from engine.AsResumer — the wrapper walks Unwrap so optional
// interfaces stay reachable.
func TestWithCapabilities_PreservesResumer(t *testing.T) {
	probed := false
	resumable := &resumerEngineForCapsTest{
		probe: func(cp engine.Checkpoint) error {
			probed = true
			return nil
		},
	}
	wrapped := engine.WithCapabilities(resumable, engine.Capabilities{SupportsResume: true})

	r, ok := engine.AsResumer(wrapped)
	if !ok {
		t.Fatalf("AsResumer(WithCapabilities(resumer, ...)) = (nil, false); want surfaced")
	}
	if err := r.CanResume(engine.Checkpoint{ExecID: "r"}); err != nil {
		t.Fatalf("CanResume returned %v", err)
	}
	if !probed {
		t.Fatalf("AsResumer must reach the underlying CanResume implementation")
	}
}

// TestSuggestCheckpoint_PreservedThroughWithCapabilities is the
// CheckpointSuggester sibling of TestWithCapabilities_PreservesResumer.
func TestSuggestCheckpoint_PreservedThroughWithCapabilities(t *testing.T) {
	se := &suggestingEngine{}
	wrapped := engine.WithCapabilities(se, engine.Capabilities{EmitsCheckpoint: true})

	if err := engine.SuggestCheckpoint(wrapped); err != nil {
		t.Fatalf("SuggestCheckpoint returned %v", err)
	}
	if se.calls != 1 {
		t.Fatalf("SuggestCheckpoint must reach the wrapped engine; calls = %d", se.calls)
	}
}

type resumerEngineForCapsTest struct {
	probe func(engine.Checkpoint) error
}

func (r *resumerEngineForCapsTest) Execute(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
	return b, nil
}

func (r *resumerEngineForCapsTest) CanResume(cp engine.Checkpoint) error {
	if r.probe == nil {
		return nil
	}
	return r.probe(cp)
}
