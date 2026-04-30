package tool

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

type metaTool struct {
	def  model.ToolDefinition
	meta ToolMeta
}

func (m *metaTool) Definition() model.ToolDefinition                    { return m.def }
func (m *metaTool) Execute(_ context.Context, _ string) (string, error) { return "ok", nil }
func (m *metaTool) Metadata() ToolMeta                                  { return m.meta }

func TestMetadataOf_DefaultZero(t *testing.T) {
	if got := MetadataOf(nil); got != (ToolMeta{}) {
		t.Errorf("MetadataOf(nil) = %+v, want zero", got)
	}
	if got := MetadataOf(stubTool("plain")); got != (ToolMeta{}) {
		t.Errorf("MetadataOf(plain) = %+v, want zero", got)
	}
}

func TestMetadataOf_DeclaredValues(t *testing.T) {
	tl := &metaTool{
		def:  model.ToolDefinition{Name: "writer"},
		meta: ToolMeta{RateLimit: 5, MutatesState: true},
	}
	got := MetadataOf(tl)
	if got.RateLimit != 5 {
		t.Errorf("RateLimit = %v, want 5", got.RateLimit)
	}
	if !got.MutatesState {
		t.Errorf("MutatesState = false, want true")
	}
}

func TestRegistry_Use_OutermostFirst(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("foo"))

	var order []string
	var mu sync.Mutex
	track := func(label string) Middleware {
		return func(next Dispatch) Dispatch {
			return func(ctx context.Context, call model.ToolCall) model.ToolResult {
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
	r.Use(track("a"), track("b"))

	res := r.Execute(context.Background(), model.ToolCall{ID: "1", Name: "foo"})
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}

	want := []string{"a:pre", "b:pre", "b:post", "a:post"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestRegistry_Use_NilSkipped(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("foo"))
	r.Use(nil, nil)

	res := r.Execute(context.Background(), model.ToolCall{ID: "1", Name: "foo"})
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
}

func TestRegistry_Use_ShortCircuit(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool("foo"))

	deny := func(_ Dispatch) Dispatch {
		return func(_ context.Context, call model.ToolCall) model.ToolResult {
			return model.ToolResult{ToolCallID: call.ID, Content: "denied", IsError: true}
		}
	}
	r.Use(deny)

	res := r.Execute(context.Background(), model.ToolCall{ID: "1", Name: "foo"})
	if !res.IsError || res.Content != "denied" {
		t.Errorf("expected denied error, got %+v", res)
	}
}

func TestRegistry_Use_SeesNotFound(t *testing.T) {
	r := NewRegistry()

	var seen string
	audit := func(next Dispatch) Dispatch {
		return func(ctx context.Context, call model.ToolCall) model.ToolResult {
			res := next(ctx, call)
			seen = res.Content
			return res
		}
	}
	r.Use(audit)

	res := r.Execute(context.Background(), model.ToolCall{ID: "1", Name: "missing"})
	if !res.IsError {
		t.Fatalf("expected error result, got %+v", res)
	}
	if !strings.Contains(seen, "missing") {
		t.Errorf("middleware should observe not-found content, got %q", seen)
	}
}
