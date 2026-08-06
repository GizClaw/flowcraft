package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/message"
)

func errTool(name string, err error) Tool {
	return FuncTool(message.Definition{Name: name}, func(_ context.Context, _ string) (string, error) {
		return "", err
	})
}

func TestNewExecutor_NilCatalogPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for nil catalog")
		}
	}()
	NewExecutor(nil)
}

func TestExecute_Success(t *testing.T) {
	r := NewRegistry()
	r.Register(FuncTool(
		message.Definition{Name: "echo"},
		func(_ context.Context, args string) (string, error) {
			return "echoed:" + args, nil
		},
	))
	exec := NewExecutor(r)

	result := exec.Execute(context.Background(), message.Call{
		ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"a":1}`),
	})
	if result.CallID != "call-1" {
		t.Errorf("CallID = %q, want %q", result.CallID, "call-1")
	}
	if result.Content != `echoed:{"a":1}` {
		t.Errorf("Content = %q, want %q", result.Content, `echoed:{"a":1}`)
	}
	if result.IsError {
		t.Error("IsError should be false for success")
	}
}

func TestExecute_ToolNotFound(t *testing.T) {
	exec := NewExecutor(NewRegistry())
	result := exec.Execute(context.Background(), message.Call{
		ID: "call-1", Name: "missing", Arguments: json.RawMessage("{}"),
	})
	if !result.IsError {
		t.Fatal("expected IsError for missing tool")
	}
	if !strings.Contains(result.Content, "not found") {
		t.Errorf("Content = %q, want to contain 'not found'", result.Content)
	}
}

func TestExecute_ToolReturnsError(t *testing.T) {
	r := NewRegistry()
	r.Register(errTool("fail", errors.New("broken")))
	exec := NewExecutor(r)

	result := exec.Execute(context.Background(), message.Call{
		ID: "call-2", Name: "fail", Arguments: json.RawMessage("{}"),
	})
	if !result.IsError {
		t.Error("IsError should be true")
	}
	if !strings.Contains(result.Content, "broken") {
		t.Errorf("Content = %q, want to contain 'broken'", result.Content)
	}
}

func TestChain_OutermostFirst(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("foo"))

	var order []string
	var mu sync.Mutex
	track := func(label string) Middleware {
		return func(next Dispatch) Dispatch {
			return func(ctx context.Context, call message.Call) message.Result {
				mu.Lock()
				order = append(order, label+":pre")
				mu.Unlock()
				res := next(ctx, call)
				mu.Lock()
				order = append(order, label+":post")
				mu.Unlock()
				return res
			}
		}
	}
	exec := NewExecutor(r, track("a"), track("b"))

	res := exec.Execute(context.Background(), message.Call{ID: "1", Name: "foo"})
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}

	want := []string{"a:pre", "b:pre", "b:post", "a:post"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestChain_NilMiddlewareSkipped(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("foo"))
	exec := NewExecutor(r, nil, nil)

	res := exec.Execute(context.Background(), message.Call{ID: "1", Name: "foo"})
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
}

func TestChain_ShortCircuit(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("foo"))

	deny := func(_ Dispatch) Dispatch {
		return func(_ context.Context, call message.Call) message.Result {
			return message.Result{CallID: call.ID, Content: "denied", IsError: true}
		}
	}
	exec := NewExecutor(r, deny)

	res := exec.Execute(context.Background(), message.Call{ID: "1", Name: "foo"})
	if !res.IsError || res.Content != "denied" {
		t.Errorf("expected denied error, got %+v", res)
	}
}

func TestChain_SeesNotFound(t *testing.T) {
	var seen string
	audit := func(next Dispatch) Dispatch {
		return func(ctx context.Context, call message.Call) message.Result {
			res := next(ctx, call)
			seen = res.Content
			return res
		}
	}
	exec := NewExecutor(NewRegistry(), audit)

	res := exec.Execute(context.Background(), message.Call{ID: "1", Name: "missing"})
	if !res.IsError {
		t.Fatalf("expected error result, got %+v", res)
	}
	if !strings.Contains(seen, "missing") {
		t.Errorf("middleware should observe not-found content, got %q", seen)
	}
}

func TestExecuteAll_Success(t *testing.T) {
	r := NewRegistry()
	r.Register(FuncTool(
		message.Definition{Name: "add"},
		func(_ context.Context, args string) (string, error) {
			return "result:" + args, nil
		},
	))
	exec := NewExecutor(r)

	calls := []message.Call{
		{ID: "c1", Name: "add", Arguments: json.RawMessage(`{"n":1}`)},
		{ID: "c2", Name: "add", Arguments: json.RawMessage(`{"n":2}`)},
		{ID: "c3", Name: "add", Arguments: json.RawMessage(`{"n":3}`)},
	}
	results := exec.ExecuteAll(context.Background(), calls)
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	for i, res := range results {
		if res.CallID != calls[i].ID {
			t.Errorf("results[%d].CallID = %q, want %q", i, res.CallID, calls[i].ID)
		}
		expected := "result:" + string(calls[i].Arguments)
		if res.Content != expected {
			t.Errorf("results[%d].Content = %q, want %q", i, res.Content, expected)
		}
	}
}

func TestExecuteAll_MixedSuccessAndFailure(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("good"))
	r.Register(errTool("bad", errors.New("fail")))
	exec := NewExecutor(r)

	calls := []message.Call{
		{ID: "c1", Name: "good", Arguments: json.RawMessage("{}")},
		{ID: "c2", Name: "bad", Arguments: json.RawMessage("{}")},
		{ID: "c3", Name: "good", Arguments: json.RawMessage("{}")},
	}
	results := exec.ExecuteAll(context.Background(), calls)
	if len(results) != 3 {
		t.Fatalf("len = %d, want 3", len(results))
	}
	if results[0].IsError {
		t.Error("results[0] should succeed")
	}
	if !results[1].IsError {
		t.Error("results[1] should be error")
	}
	if results[2].IsError {
		t.Error("results[2] should succeed")
	}
}

func TestExecuteAll_Empty(t *testing.T) {
	exec := NewExecutor(NewRegistry())
	results := exec.ExecuteAll(context.Background(), nil)
	if len(results) != 0 {
		t.Errorf("len = %d, want 0", len(results))
	}
}

func TestExecuteAll_ToolNotFound(t *testing.T) {
	exec := NewExecutor(NewRegistry())
	results := exec.ExecuteAll(context.Background(), []message.Call{
		{ID: "c1", Name: "nonexistent", Arguments: json.RawMessage("{}")},
	})
	if len(results) != 1 {
		t.Fatalf("len = %d, want 1", len(results))
	}
	if !results[0].IsError {
		t.Error("should be error for missing tool")
	}
}

func TestExecute_ContextCancelled(t *testing.T) {
	r := NewRegistry()
	r.Register(FuncTool(
		message.Definition{Name: "slow"},
		func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	))
	exec := NewExecutor(r)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := exec.Execute(ctx, message.Call{
		ID: "call-3", Name: "slow", Arguments: json.RawMessage("{}"),
	})
	if !result.IsError {
		t.Error("IsError should be true for cancelled context")
	}
}

func TestExecuteAll_ConcurrencyUnboundedByDefault(t *testing.T) {
	// Without a concurrency middleware every call starts immediately:
	// gate the tool so the test only completes when all calls are
	// in-flight at once.
	const n = 8
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	r := NewRegistry()
	r.Register(FuncTool(
		message.Definition{Name: "gated"},
		func(ctx context.Context, _ string) (string, error) {
			entered <- struct{}{}
			select {
			case <-release:
				return "ok", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	))
	exec := NewExecutor(r)

	calls := make([]message.Call, n)
	for i := range calls {
		calls[i] = message.Call{ID: fmt.Sprintf("c%d", i), Name: "gated", Arguments: json.RawMessage("{}")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan []message.Result, 1)
	go func() { done <- exec.ExecuteAll(ctx, calls) }()

	for i := 0; i < n; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatalf("only %d/%d calls in-flight; fan-out serialized", i, n)
		}
	}
	close(release)
	results := <-done
	for i, res := range results {
		if res.IsError {
			t.Errorf("results[%d] unexpected error: %s", i, res.Content)
		}
	}
}
