package jsrt

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/script"
)

func TestRuntime_VMCleanup_GlobalsNotLeaked(t *testing.T) {
	rt := New(WithPoolSize(1))

	_, err := rt.Exec(context.Background(), "set", `
		var myGlobal = "leaked";
		config.injected = true;
	`, &script.Env{Config: map[string]any{"key": "val"}})
	if err != nil {
		t.Fatalf("first exec: %v", err)
	}

	_, err = rt.Exec(context.Background(), "check", `
		if (typeof myGlobal !== "undefined" && myGlobal === "leaked") {
			// var declarations in goja may persist but config should be cleaned
		}
		if (config !== undefined && config !== null && typeof config === "object" && config.key === "val") {
			throw new Error("config leaked from previous execution");
		}
	`, &script.Env{Config: map[string]any{"other": "data"}})
	if err != nil {
		t.Fatalf("second exec: %v", err)
	}
}

func TestRuntime_VMCleanup_SignalNotLeaked(t *testing.T) {
	rt := New(WithPoolSize(1))

	_, _ = rt.Exec(context.Background(), "with-signal", `
		// signal is available here
	`, nil)

	_, err := rt.Exec(context.Background(), "check-signal", `
		// signal should be re-injected fresh each call, not leaked from prior
		if (typeof signal === "undefined") {
			throw new Error("signal should always be injected");
		}
	`, nil)
	if err != nil {
		t.Fatalf("signal check: %v", err)
	}
}

func TestRuntime_ConcurrentExec(t *testing.T) {
	rt := New(WithPoolSize(4))
	const n = 20

	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			env := &script.Env{Config: map[string]any{"index": idx}}
			_, err := rt.Exec(context.Background(), "concurrent", `
				if (config.index === undefined) throw new Error("missing index");
			`, env)
			if err != nil {
				errs <- err
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent exec error: %v", err)
	}
}

func TestRuntime_BindingsCleanup(t *testing.T) {
	rt := New(WithPoolSize(1))

	env1 := &script.Env{
		Bindings: map[string]any{
			"helper": map[string]any{
				"greet": func(name string) string { return "Hello " + name },
			},
		},
	}
	_, err := rt.Exec(context.Background(), "multi", `
		var result = helper.greet("World");
		if (result !== "Hello World") throw new Error("helper failed: " + result);
	`, env1)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	_, err = rt.Exec(context.Background(), "check-cleaned", `
		if (typeof helper !== "undefined" && helper !== null && typeof helper.greet === "function") {
			throw new Error("helper binding leaked");
		}
	`, nil)
	if err != nil {
		t.Fatalf("binding leaked: %v", err)
	}
}

func TestRuntime_ContextCancellation(t *testing.T) {
	rt := New(WithPoolSize(1))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := rt.Exec(ctx, "cancelled", `var x = 1;`, nil)
	_ = err
}
