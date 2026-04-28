package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// Tests live in the internal "agent" package because they probe
// defaultSeeder, which is unexported. Other agent_test.go files use
// the public API via "agent_test" — that boundary is intentional.

func TestDefaultSeeder_AppendsRequestMessage(t *testing.T) {
	req := &Request{Message: model.NewTextMessage(model.RoleUser, "hi")}

	b, err := defaultSeeder{}.SeedBoard(context.Background(), RunInfo{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := b.Channel(engine.MainChannel)
	if len(got) != 1 || got[0].Content() != "hi" {
		t.Errorf("MainChannel = %+v, want [hi]", got)
	}
}

func TestDefaultSeeder_CopiesInputsToVars(t *testing.T) {
	req := &Request{
		Message: model.NewTextMessage(model.RoleUser, "hi"),
		Inputs:  map[string]any{"a": 1, "b": "two"},
	}

	b, err := defaultSeeder{}.SeedBoard(context.Background(), RunInfo{}, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, _ := b.GetVar("a"); v != 1 {
		t.Errorf("vars[a] = %v, want 1", v)
	}
	if v, _ := b.GetVar("b"); v != "two" {
		t.Errorf("vars[b] = %v, want two", v)
	}
}

func TestDefaultSeeder_FreshBoardEachCall(t *testing.T) {
	req := &Request{Message: model.NewTextMessage(model.RoleUser, "hi")}

	b1, _ := defaultSeeder{}.SeedBoard(context.Background(), RunInfo{}, req)
	b2, _ := defaultSeeder{}.SeedBoard(context.Background(), RunInfo{}, req)

	if b1 == b2 {
		t.Error("defaultSeeder must return a fresh Board each call")
	}
}

func TestBoardSeederFunc_Adapts(t *testing.T) {
	called := false
	f := BoardSeederFunc(func(_ context.Context, info RunInfo, req *Request) (*engine.Board, error) {
		called = true
		if info.RunID != "r-1" {
			t.Errorf("RunInfo.RunID = %q, want r-1", info.RunID)
		}
		if req.Message.Content() != "hello" {
			t.Errorf("req.Message = %q, want hello", req.Message.Content())
		}
		return engine.NewBoard(), nil
	})

	_, err := f.SeedBoard(context.Background(),
		RunInfo{RunID: "r-1"},
		&Request{Message: model.NewTextMessage(model.RoleUser, "hello")},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("BoardSeederFunc.SeedBoard did not invoke the wrapped function")
	}
}

func TestBoardSeederFunc_PropagatesError(t *testing.T) {
	boom := errors.New("boom")
	f := BoardSeederFunc(func(context.Context, RunInfo, *Request) (*engine.Board, error) {
		return nil, boom
	})

	b, err := f.SeedBoard(context.Background(), RunInfo{}, &Request{})
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want %v", err, boom)
	}
	if b != nil {
		t.Errorf("board should be nil on error; got %+v", b)
	}
}
