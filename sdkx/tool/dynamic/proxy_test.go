package dynamic

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
)

type closableFuncTool struct {
	sdktool.Tool
	closed *bool
}

func (c closableFuncTool) Close() error {
	*c.closed = true
	return nil
}

func TestLazyTool_SingleflightConcurrentLoad(t *testing.T) {
	var calls atomic.Int32
	proxy := NewLazyTool(nil, "p", func(_ context.Context) (sdktool.Tool, error) {
		calls.Add(1)
		return funcTool("p", "real"), nil
	})

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = proxy.EnsureLoaded(context.Background())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureLoaded[%d] = %v", i, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("loader calls = %d, want 1 (singleflight)", calls.Load())
	}
}

func TestLazyTool_RetriesWithinCall(t *testing.T) {
	var calls atomic.Int32
	proxy := NewLazyTool(nil, "p", func(_ context.Context) (sdktool.Tool, error) {
		if calls.Add(1) < 2 {
			return nil, errors.New("transient")
		}
		return funcTool("p", "real"), nil
	}, WithRetryPolicy(RetryPolicy{Attempts: 3, BaseDelay: 0, MaxDelay: 0}))

	if err := proxy.EnsureLoaded(context.Background()); err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("loader calls = %d, want 2 (first attempt failed)", calls.Load())
	}
}

func TestLazyTool_FailedLoadRetriesOnNextCall(t *testing.T) {
	var calls atomic.Int32
	proxy := NewLazyTool(nil, "p", func(_ context.Context) (sdktool.Tool, error) {
		calls.Add(1)
		return nil, errors.New("always fails")
	}, WithRetryPolicy(RetryPolicy{Attempts: 2, BaseDelay: 0, MaxDelay: 0}))

	if err := proxy.EnsureLoaded(context.Background()); err == nil {
		t.Fatal("first EnsureLoaded succeeded, want error")
	}
	if proxy.LastError() == nil {
		t.Fatal("LastError is nil after failed load")
	}
	if err := proxy.EnsureLoaded(context.Background()); err == nil {
		t.Fatal("second EnsureLoaded succeeded, want error")
	}
	if calls.Load() != 4 {
		t.Errorf("loader calls = %d, want 4 (retry across two calls)", calls.Load())
	}
}

func TestLazyTool_PlaceholderThenRealDefinition(t *testing.T) {
	proxy := NewLazyTool(nil, "p", func(_ context.Context) (sdktool.Tool, error) {
		return funcTool("p", "real description"), nil
	})

	before := proxy.Definition()
	if before.Name != "p" || before.Description != "" {
		t.Errorf("placeholder = %+v, want name p with empty description", before)
	}
	if err := proxy.EnsureLoaded(context.Background()); err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	after := proxy.Definition()
	if after.Description != "real description" {
		t.Errorf("loaded definition = %+v, want real description", after)
	}
}

func TestLazyTool_MetadataDelegatesAfterLoad(t *testing.T) {
	type metaTool struct {
		sdktool.Tool
	}
	inner := funcTool("p", "x")
	proxy := NewLazyTool(nil, "p", func(_ context.Context) (sdktool.Tool, error) {
		return metaTool{Tool: inner}, nil
	})
	if got := proxy.Metadata(); got != (sdktool.ToolMeta{}) {
		t.Errorf("unloaded metadata = %+v, want zero", got)
	}
}

func TestLazyTool_ExecuteLoadsAndForwards(t *testing.T) {
	var loads atomic.Int32
	proxy := NewLazyTool(nil, "p", func(_ context.Context) (sdktool.Tool, error) {
		loads.Add(1)
		return funcTool("p", "real"), nil
	})
	res, err := proxy.Execute(context.Background(), "{}")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res != "ran:p" {
		t.Errorf("Execute result = %q, want ran:p", res)
	}
	if loads.Load() != 1 {
		t.Errorf("loader calls = %d, want 1", loads.Load())
	}
}

func TestLazyTool_ExecuteFailureIsNotAvailable(t *testing.T) {
	proxy := NewLazyTool(nil, "p", func(_ context.Context) (sdktool.Tool, error) {
		return nil, errors.New("down")
	}, WithRetryPolicy(RetryPolicy{Attempts: 1, BaseDelay: 0}))
	_, err := proxy.Execute(context.Background(), "{}")
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("Execute error = %v, want NotAvailable", err)
	}
}

func TestLazyTool_CloseClosesInnerAndForbidsLoad(t *testing.T) {
	closed := false
	inner := closableFuncTool{Tool: funcTool("p", "real"), closed: &closed}
	proxy := NewLazyTool(nil, "p", func(_ context.Context) (sdktool.Tool, error) {
		return inner, nil
	})
	if err := proxy.EnsureLoaded(context.Background()); err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Error("inner tool was not closed")
	}
	if err := proxy.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := proxy.EnsureLoaded(context.Background()); !errdefs.IsNotAvailable(err) {
		t.Fatalf("EnsureLoaded after close = %v, want NotAvailable", err)
	}
	if _, err := proxy.Execute(context.Background(), "{}"); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Execute after close = %v, want NotAvailable", err)
	}
}

func TestLazyTool_ForwardsToRegistryReplacement(t *testing.T) {
	reg := sdktool.NewRegistry()
	proxy := NewLazyTool(reg, "p", func(_ context.Context) (sdktool.Tool, error) {
		return funcTool("p", "old"), nil
	})
	reg.Register(proxy)
	if err := proxy.EnsureLoaded(context.Background()); err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	replacement := funcTool("p", "new")
	reg.Register(replacement)
	if got := proxy.Definition().Description; got != "new" {
		t.Errorf("definition = %q, want registry replacement", got)
	}
	if res, err := proxy.Execute(context.Background(), "{}"); err != nil || res != "ran:p" {
		t.Errorf("Execute = %q, %v; want registry replacement", res, err)
	}
}

func TestLazyTool_RejectsNilLoader(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for nil loader")
		}
	}()
	NewLazyTool(nil, "p", nil)
}
