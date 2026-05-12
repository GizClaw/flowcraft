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

// TestAPI_Resume_MissingRunID asserts the body validation: an
// empty run_id surfaces 400 Validation rather than dispatching a
// resume against an unspecified run.
func TestAPI_Resume_MissingRunID(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		"/v1/vessels/support/resume",
		strings.NewReader(`{}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d body=%s, want 400", w.Code, w.Body.String())
	}
	var body errorBody
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Reason != "Validation" {
		t.Fatalf("reason = %q, want Validation", body.Reason)
	}
}

// TestAPI_Resume_NoStoreReturnsServiceUnavailable: the test
// server has no CheckpointStore wired (catalog default), so a
// well-formed Resume request must surface 503 NotAvailable —
// pinning the route's error mapping for the most common
// misconfiguration.
func TestAPI_Resume_NoStoreReturnsServiceUnavailable(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		"/v1/vessels/support/resume",
		strings.NewReader(`{"run_id":"any"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d body=%s, want 503", w.Code, w.Body.String())
	}
}

// TestAPI_Resume_UnknownVessel: routing mismatch surfaces 404
// before the body is even examined.
func TestAPI_Resume_UnknownVessel(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		"/v1/vessels/ghost/resume",
		strings.NewReader(`{"run_id":"any"}`)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d body=%s, want 404", w.Code, w.Body.String())
	}
}
