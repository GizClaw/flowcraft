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
