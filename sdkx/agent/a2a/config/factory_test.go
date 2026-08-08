package config_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/agent/agenttest"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdkx/agent/a2a"
	a2aconfig "github.com/GizClaw/flowcraft/sdkx/agent/a2a/config"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

func TestSpec(t *testing.T) {
	spec := a2aconfig.NewFactory().Spec()
	if spec.Kind != a2aconfig.Kind {
		t.Errorf("Kind = %q, want %q", spec.Kind, a2aconfig.Kind)
	}
	if spec.Impl != "" {
		t.Errorf("Impl = %q, want empty (engine kind)", spec.Impl)
	}
}

func TestCapabilities(t *testing.T) {
	caps := a2aconfig.NewFactory().Capabilities()
	if !caps.SupportsResume || !caps.EmitsCheckpoint || !caps.EmitsUserPrompt {
		t.Errorf("Capabilities = %+v, want all three claimed", caps)
	}
}

func TestNew_StrictDecodeRejectsUnknownKeys(t *testing.T) {
	_, err := a2aconfig.NewFactory().New(context.Background(), sdkconfig.Input{
		Settings: json.RawMessage(`{"url":"http://x","bogus":1}`),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation for unknown setting", err)
	}
}

func TestNew_RequiresEndpoint(t *testing.T) {
	_, err := a2aconfig.NewFactory().New(context.Background(), sdkconfig.Input{
		Settings: json.RawMessage(`{}`),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation when no url/card_url/card", err)
	}
}

// rpcServer serves both the AgentCard discovery endpoint and the JSON-RPC
// endpoint, recording the Authorization header.
func rpcServer(t *testing.T, streaming bool, wantAuth string) (*httptest.Server, *string) {
	t.Helper()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/agent-card.json":
			card := map[string]any{
				"name": "discovered", "description": "d", "version": "1.0.0",
				"protocolVersion":    "1.0",
				"defaultInputModes":  []string{"text/plain"},
				"defaultOutputModes": []string{"text/plain"},
				"skills":             []any{},
				"capabilities":       map[string]any{"streaming": streaming},
				"supportedInterfaces": []any{map[string]any{
					"url": "http://" + r.Host, "protocolBinding": "JSONRPC",
					"protocolVersion": "1.0",
				}},
			}
			_ = json.NewEncoder(w).Encode(card)
		case r.Method == http.MethodPost:
			gotAuth = r.Header.Get("Authorization")
			var req struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			var taskID string
			if req.Method == "SendMessage" {
				taskID = "t1"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": "r1",
				"result": map[string]any{
					"task": map[string]any{
						"id": taskID, "contextId": "c1",
						"status": map[string]any{"state": "TASK_STATE_COMPLETED"},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &gotAuth
}

func newEngineFromSettings(t *testing.T, settings string) *a2a.Engine {
	t.Helper()
	got, err := a2aconfig.NewFactory().New(context.Background(), sdkconfig.Input{
		Settings: json.RawMessage(settings),
	})
	if err != nil {
		t.Fatalf("factory.New: %v", err)
	}
	eng, ok := got.(*a2a.Engine)
	if !ok {
		t.Fatalf("factory.New returned %T, want *a2a.Engine", got)
	}
	return eng
}

func runTurnAgainst(t *testing.T, eng agent.Engine) *agent.Result {
	t.Helper()
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "cfg-test"}, eng, agent.Request{
		Message: message.Message{Role: message.RoleUser,
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}}},
	}, agent.WithHost(agenttest.NewMockHost()))
	if err != nil {
		t.Fatalf("agent.Execute: %v", err)
	}
	return res
}

func TestNew_URLEndpointWithAuth(t *testing.T) {
	srv, gotAuth := rpcServer(t, false, "Bearer secret")
	settings := `{"url":"` + srv.URL + `","auth":{"scheme":"bearer","token":"secret"}}`
	eng := newEngineFromSettings(t, settings)
	res := runTurnAgainst(t, eng)
	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	if *gotAuth != "Bearer secret" {
		t.Errorf("Authorization header = %q, want Bearer secret", *gotAuth)
	}
}

func TestNew_CardURLDiscovery(t *testing.T) {
	srv, _ := rpcServer(t, false, "")
	settings := `{"card_url":"` + srv.URL + `"}`
	eng := newEngineFromSettings(t, settings)
	res := runTurnAgainst(t, eng)
	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	if card := eng.Card(); card == nil || card.Name != "discovered" {
		t.Errorf("discovered card = %+v, want name discovered", card)
	}
}

func TestNew_InlineCard(t *testing.T) {
	card := map[string]any{
		"name": "inline", "description": "d", "version": "1.0.0",
		"defaultInputModes":  []string{"text/plain"},
		"defaultOutputModes": []string{"text/plain"},
		"skills":             []any{},
		"capabilities":       map[string]any{},
		"supportedInterfaces": []any{map[string]any{
			"url": "http://127.0.0.1:1", "protocolBinding": "JSONRPC", "protocolVersion": "1.0",
		}},
	}
	raw, _ := json.Marshal(card)
	eng := newEngineFromSettings(t, `{"card":`+string(raw)+`}`)
	if eng.Card() == nil || eng.Card().Name != "inline" {
		t.Fatalf("inline card not used: %+v", eng.Card())
	}
}

func TestNew_Protocol03URL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		// 0.3 wire: kind-discriminated task with a lowercase state.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": "r1",
			"result": map[string]any{
				"kind": "task", "id": "t03", "contextId": "c03",
				"status": map[string]any{"state": "completed"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	eng := newEngineFromSettings(t, `{"url":"`+srv.URL+`","protocol":"0.3"}`)
	res := runTurnAgainst(t, eng)
	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed over 0.3 wire", res.Status)
	}
}

func TestNew_InvalidProtocol(t *testing.T) {
	_, err := a2aconfig.NewFactory().New(context.Background(), sdkconfig.Input{
		Settings: json.RawMessage(`{"url":"http://x","protocol":"2.0"}`),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation for unsupported protocol", err)
	}
}

func TestDeployRegistration(t *testing.T) {
	srv, _ := rpcServer(t, false, "")
	builder := deploy.NewBuilder()
	builder.MustRegisterEngine(a2aconfig.NewFactory())
	doc := deploy.Document{
		Version: "v1",
		Agents: map[string]deploy.AgentEntry{
			"remote": {
				Card: struct {
					Name        string `json:"name,omitempty"`
					Description string `json:"description,omitempty"`
				}{Name: "remote"},
				Engine: deploy.EngineEntry{
					Kind:     a2aconfig.Kind,
					Settings: json.RawMessage(`{"url":"` + srv.URL + `"}`),
				},
			},
		},
	}
	result, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("deploy Build: %v", err)
	}
	inst, ok := result.Instance("remote")
	if !ok {
		t.Fatalf("agent instance remote not found")
	}
	if _, ok := inst.Engine.(*a2a.Engine); !ok {
		t.Fatalf("engine = %T, want *a2a.Engine", inst.Engine)
	}
}

func TestNew_AuthValidation(t *testing.T) {
	cases := []string{
		`{"url":"http://x","auth":{"scheme":"bearer"}}`,
		`{"url":"http://x","auth":{"scheme":"basic","username":"u"}}`,
		`{"url":"http://x","auth":{"scheme":"custom","header":"X-K"}}`,
		`{"url":"http://x","auth":{"scheme":"weird","token":"t"}}`,
	}
	for _, settings := range cases {
		_, err := a2aconfig.NewFactory().New(context.Background(), sdkconfig.Input{
			Settings: json.RawMessage(settings),
		})
		if !errdefs.IsValidation(err) {
			t.Errorf("settings %s: err = %v, want Validation", settings, err)
		}
	}
}

func TestNew_OptionValidation(t *testing.T) {
	cases := []string{
		`{"url":"http://x","poll_interval":"nope"}`,
		`{"url":"http://x","poll_interval":"0s"}`,
		`{"url":"http://x","preferred_transports":["carrier-pigeon"]}`,
	}
	for _, settings := range cases {
		_, err := a2aconfig.NewFactory().New(context.Background(), sdkconfig.Input{
			Settings: json.RawMessage(settings),
		})
		if !errdefs.IsValidation(err) {
			t.Errorf("settings %s: err = %v, want Validation", settings, err)
		}
	}
}

func TestNew_InlineCardWithoutInterfaces(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"name": "x", "version": "1.0.0"})
	_, err := a2aconfig.NewFactory().New(context.Background(), sdkconfig.Input{
		Settings: json.RawMessage(`{"card":` + string(raw) + `}`),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation for interface-less inline card", err)
	}
}

func TestNew_BasicAuth(t *testing.T) {
	srv, gotAuth := rpcServer(t, false, "")
	settings := `{"url":"` + srv.URL + `","auth":{"scheme":"basic","username":"u","password":"p"}}`
	eng := newEngineFromSettings(t, settings)
	if res := runTurnAgainst(t, eng); res.Status != agent.StatusCompleted {
		t.Fatalf("basic auth turn status = %q", res.Status)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	if *gotAuth != want {
		t.Errorf("Authorization = %q, want %q", *gotAuth, want)
	}
}

func TestNew_CustomHeaderAuth(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/agent-card.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "d", "description": "d", "version": "1.0.0",
				"defaultInputModes":  []string{"text/plain"},
				"defaultOutputModes": []string{"text/plain"},
				"skills":             []any{},
				"supportedInterfaces": []any{map[string]any{
					"url": "http://" + r.Host, "protocolBinding": "JSONRPC", "protocolVersion": "1.0",
				}},
			})
		case r.Method == http.MethodPost:
			gotKey = r.Header.Get("X-API-Key")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": "r1",
				"result": map[string]any{"task": map[string]any{
					"id": "t1", "contextId": "c1",
					"status": map[string]any{"state": "TASK_STATE_COMPLETED"},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	settings := `{"card_url":"` + srv.URL + `","auth":{"scheme":"custom","header":"X-API-Key","value":"k123"}}`
	eng := newEngineFromSettings(t, settings)
	if res := runTurnAgainst(t, eng); res.Status != agent.StatusCompleted {
		t.Fatalf("custom auth turn status = %q", res.Status)
	}
	if gotKey != "k123" {
		t.Errorf("X-API-Key = %q, want k123", gotKey)
	}
}
