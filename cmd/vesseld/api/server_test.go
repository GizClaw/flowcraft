package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/fleet"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/model"
)

const apiConfig = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-default
spec:
  control:
    socket: /tmp/v.sock
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: support
spec:
  agents: [helper]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: helper
spec:
  engine:
    ref: noop
`

func newTestServer(t *testing.T) *Server {
	t.Helper()
	objs, err := apispec.DecodeAll(strings.NewReader(apiConfig), "<test>")
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog.New()
	cat.RegisterEngine("noop", func(_ string, _ map[string]any, _ catalog.Deps) (engine.Engine, error) {
		return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ack"))
			return b, nil
		}), nil
	})
	plan, errs := resolver.Resolve(objs, cat, resolver.ResolveOptions{})
	if errs.Len() != 0 {
		t.Fatalf("resolve: %v", errs)
	}
	f, err := fleet.Build(*plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Stop(context.Background()) })

	s := New(Config{Version: "test"}, f)
	s.MarkReady()
	return s
}

func TestAPI_HealthAndReady(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	for _, route := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, route, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d", route, w.Code)
		}
	}
}

func TestAPI_VesselsList(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/vessels", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var got []map[string]string
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["name"] != "support" {
		t.Fatalf("got %v", got)
	}
}

func TestAPI_Call_RoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"agent":"helper","query":"hi"}`)
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/vessels/support/call", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "completed" {
		t.Fatalf("status field = %v", got["status"])
	}
}

func TestAPI_NotFound(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/vessels/missing/phase", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d", w.Code)
	}
}
