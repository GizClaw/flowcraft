package luart

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
)

func TestDeployFactorySpec(t *testing.T) {
	want := sdkconfig.Spec{Kind: ResourceKind, Impl: "lua"}
	if got := NewDeployFactory().Spec(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestDeployFactoryBuildsDefaultAndConfiguredRuntime(t *testing.T) {
	tests := []struct {
		name       string
		settings   string
		configured bool
	}{
		{name: "default"},
		{
			name: "configured",
			settings: `{
				"pool_size": 1,
				"max_exec_time": "50ms"
			}`,
			configured: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := NewDeployFactory().New(context.Background(), sdkconfig.Input{
				Settings: settingsJSON(t, tt.settings),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			rt, ok := value.(*Runtime)
			if !ok {
				t.Fatalf("New returned %T, want *luart.Runtime", value)
			}
			defer func() { _ = rt.Close() }()
			if tt.configured {
				if rt.poolSize != 1 || rt.maxExecTime != 50*time.Millisecond {
					t.Fatalf("runtime settings = pool %d, duration %s",
						rt.poolSize, rt.maxExecTime)
				}
				if rt.SupportsNestedExec() {
					t.Fatal("configured single-state runtime supports nested execution")
				}
			}
			if _, err := rt.Exec(context.Background(), "smoke", `local x = 1`, nil); err != nil {
				t.Fatalf("Exec: %v", err)
			}
		})
	}
}

func TestDeployFactoryRejectsInvalidSettings(t *testing.T) {
	tests := []string{
		`{"unknown":true}`,
		`{"pool_size":0}`,
		`{"pool_size":-1}`,
		`{"max_exec_time":"nonsense"}`,
		`{"max_exec_time":"-1s"}`,
	}
	for _, settings := range tests {
		t.Run(settings, func(t *testing.T) {
			if _, err := NewDeployFactory().New(context.Background(), sdkconfig.Input{
				Settings: settingsJSON(t, settings),
			}); err == nil {
				t.Fatal("New accepted invalid settings")
			}
		})
	}
}

func TestDeployFactoryAcceptsZeroExecTime(t *testing.T) {
	value, err := NewDeployFactory().New(context.Background(), sdkconfig.Input{
		Settings: settingsJSON(t, `{"max_exec_time":"0s"}`),
	})
	if err != nil {
		t.Fatalf("New rejected zero max_exec_time: %v", err)
	}
	_ = value.(*Runtime).Close()
}

func TestRuntimeCloseWithCheckedOutVM(t *testing.T) {
	runtime := New(WithPoolSize(1))
	state, err := runtime.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	waiter := make(chan error, 1)
	go func() {
		_, err := runtime.acquire(context.Background())
		waiter <- err
	}()

	closed := make(chan error, 1)
	go func() { closed <- runtime.Close() }()
	if err := <-waiter; !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("blocked acquire after Close = %v, want ErrRuntimeClosed", err)
	}

	runtime.release(state)
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := runtime.acquire(context.Background()); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("acquire after Close = %v, want ErrRuntimeClosed", err)
	}
}

func TestRuntimeCloseCancelsActiveExec(t *testing.T) {
	runtime := New(WithPoolSize(1))
	runtime.init()
	execDone := make(chan error, 1)
	go func() {
		_, err := runtime.Exec(context.Background(), "loop", `while true do end`, nil)
		execDone <- err
	}()

	deadline := time.After(2 * time.Second)
	for len(runtime.pool) != 0 {
		select {
		case <-deadline:
			t.Fatal("Exec did not acquire the VM")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-execDone:
		if err == nil {
			t.Fatal("active Exec returned nil after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active Exec did not stop before Close returned")
	}
}

func settingsJSON(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	if raw == "" {
		return nil
	}
	var out json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return out
}
