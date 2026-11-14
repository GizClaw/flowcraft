package deploy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/goccy/go-json"
)

// hookKind is a hook the tests can register: it validates and builds nothing.
func hookKind(t *testing.T, seen *json.RawMessage) deploy.HookKind {
	t.Helper()
	return deploy.HookKind{
		Name: "probe",
		Decode: func(settings json.RawMessage) (any, error) {
			var decoded struct {
				Recap int `json:"recap"`
			}
			if err := json.Unmarshal(settings, &decoded); err != nil {
				return nil, err
			}
			if decoded.Recap < 0 {
				return nil, errors.New("recap must not be negative")
			}
			*seen = settings
			return decoded, nil
		},
		Build: func(context.Context, deploy.HookDeps) (agent.Hooks, error) {
			return agent.Hooks{}, nil
		},
	}
}

func remoteKind() deploy.EngineKind {
	return deploy.EngineKind{
		Name:  "remote",
		Local: false,
		Build: func(context.Context, deploy.EngineDeps) (agent.RunnerFactory, error) {
			return func(context.Context, string, string) (agent.Runner, error) { return nil, nil }, nil
		},
	}
}

// hookDocument is a deployment with one agent and the given hooks block.
func hookDocument(hooks string) []byte {
	return []byte(`
version: 1
agents:
  - name: seat_5
    engine: {kind: remote}
    hooks:
` + hooks)
}

func TestLoaderCarriesHooksInOrder(t *testing.T) {
	var seen json.RawMessage
	loader := deploy.Loader{Kinds: deploy.BuiltinKinds().WithEngines(remoteKind()).WithHooks(hookKind(t, &seen))}
	specs, err := loader.Load(context.Background(), hookDocument(`      - kind: probe
        settings: {recap: 12}
      - kind: probe
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(specs) != 1 || len(specs[0].Hooks) != 2 {
		t.Fatalf("hooks = %+v", specs)
	}
	if specs[0].Hooks[0].Kind != "probe" || string(specs[0].Hooks[0].Settings) != `{"recap":12}` {
		t.Fatalf("first hook = %+v", specs[0].Hooks[0])
	}
	// An omitted settings block is the empty object a kind expects, not nil.
	if string(specs[0].Hooks[1].Settings) != "{}" {
		t.Fatalf("second hook settings = %s", specs[0].Hooks[1].Settings)
	}
	if string(seen) == "" {
		t.Fatal("the loader must run the kind's Decode")
	}
}

func TestLoaderRejectsAnUnknownHookKind(t *testing.T) {
	loader := deploy.Loader{Kinds: deploy.BuiltinKinds().WithEngines(remoteKind())}
	_, err := loader.Load(context.Background(), hookDocument(`      - kind: memory
`))
	if err == nil {
		t.Fatal("an unregistered hook kind must fail the load")
	}
	if !strings.Contains(err.Error(), "hooks[0]") || !strings.Contains(err.Error(), "offered") {
		t.Fatalf("error %v does not name the entry and the registered kinds", err)
	}
}

func TestLoaderRejectsAHookWithoutAKind(t *testing.T) {
	loader := deploy.Loader{Kinds: deploy.BuiltinKinds().WithEngines(remoteKind())}
	_, err := loader.Load(context.Background(), hookDocument(`      - settings: {recap: 1}
`))
	if err == nil || !strings.Contains(err.Error(), "kind is required") {
		t.Fatalf("error = %v, want a required kind", err)
	}
}

func TestLoaderReportsWhatTheKindRejects(t *testing.T) {
	var seen json.RawMessage
	loader := deploy.Loader{Kinds: deploy.BuiltinKinds().WithEngines(remoteKind()).WithHooks(hookKind(t, &seen))}
	_, err := loader.Load(context.Background(), hookDocument(`      - kind: probe
        settings: {recap: -1}
`))
	if err == nil || !strings.Contains(err.Error(), "recap must not be negative") {
		t.Fatalf("error = %v, want the kind's message", err)
	}
}
