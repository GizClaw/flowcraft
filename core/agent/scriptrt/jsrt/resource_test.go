package jsrt

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/resource"
)

func TestDeployFactorySpec(t *testing.T) {
	want := resource.Spec{Kind: ResourceKind, Impl: "js"}
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
				"max_call_stack_size": 64,
				"max_exec_time": "50ms"
			}`,
			configured: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := NewDeployFactory().New(context.Background(), resource.Input{
				Settings: settingsJSON(t, tt.settings),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			rt, ok := value.(*Runtime)
			if !ok {
				t.Fatalf("New returned %T, want *jsrt.Runtime", value)
			}
			if tt.configured {
				if rt.poolSize != 1 || rt.maxCallStackSize != 64 ||
					rt.maxExecTime != 50*time.Millisecond {
					t.Fatalf(
						"runtime settings = pool %d, stack %d, duration %s",
						rt.poolSize, rt.maxCallStackSize, rt.maxExecTime)
				}
				if rt.SupportsNestedExec() {
					t.Fatal("configured single-VM runtime supports nested execution")
				}
			}
			if _, err := rt.Exec(context.Background(), "smoke", `var x = 1`, nil); err != nil {
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
		`{"max_call_stack_size":0}`,
		`{"max_call_stack_size":-1}`,
		`{"max_exec_time":"nonsense"}`,
		`{"max_exec_time":"-1s"}`,
	}
	for _, settings := range tests {
		t.Run(settings, func(t *testing.T) {
			if _, err := NewDeployFactory().New(context.Background(), resource.Input{
				Settings: settingsJSON(t, settings),
			}); err == nil {
				t.Fatal("New accepted invalid settings")
			}
		})
	}
}

func TestDeployFactoryAcceptsZeroExecTime(t *testing.T) {
	_, err := NewDeployFactory().New(context.Background(), resource.Input{
		Settings: settingsJSON(t, `{"max_exec_time":"0s"}`),
	})
	if err != nil {
		t.Fatalf("New rejected zero max_exec_time: %v", err)
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
