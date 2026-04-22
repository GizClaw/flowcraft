package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/eventlogtest"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

type captureSink struct {
	mu      sync.Mutex
	letters []projection.DeadLetter
}

func (c *captureSink) Write(_ context.Context, dl projection.DeadLetter) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.letters = append(c.letters, dl)
	return nil
}

func (c *captureSink) Snapshot() []projection.DeadLetter {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]projection.DeadLetter, len(c.letters))
	copy(out, c.letters)
	return out
}

func newTestEnvelope(t *testing.T, endpointID string) eventlog.Envelope {
	t.Helper()
	body := eventlog.WebhookInboundBody{
		Body:        `{"hello":"world"}`,
		ContentType: "application/json",
		EndpointID:  endpointID,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return eventlog.Envelope{
		Seq:       42,
		Type:      eventlog.EventTypeWebhookInboundReceived,
		Partition: eventlog.PartitionWebhook(endpointID),
		Payload:   raw,
	}
}

func TestRouter_Apply_2xxIsAck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	reg := NewDefaultRouteRegistry()
	reg.Register(&WebhookRoute{EndpointID: "ep-1", URL: srv.URL, Method: http.MethodPost})

	sink := &captureSink{}
	r := NewWebhookRouter(eventlogtest.NewMemoryLog(), reg, Options{DLT: sink})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	env := newTestEnvelope(t, "ep-1")
	if err := r.Apply(ctx, nil, env); err != nil {
		t.Fatalf("Apply 2xx: %v", err)
	}
	if got := sink.Snapshot(); len(got) != 0 {
		t.Fatalf("no DLT expected on 2xx, got %d", len(got))
	}
}

func TestRouter_Apply_4xxWritesDLT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"err":"bad"}`))
	}))
	defer srv.Close()

	reg := NewDefaultRouteRegistry()
	reg.Register(&WebhookRoute{EndpointID: "ep-2", URL: srv.URL, Method: http.MethodPost})

	sink := &captureSink{}
	r := NewWebhookRouter(eventlogtest.NewMemoryLog(), reg, Options{DLT: sink})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	env := newTestEnvelope(t, "ep-2")
	if err := r.Apply(ctx, nil, env); err != nil {
		t.Fatalf("Apply 4xx should not return error (ack), got: %v", err)
	}
	got := sink.Snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 DLT entry, got %d", len(got))
	}
	if got[0].Seq != 42 {
		t.Fatalf("DLT seq mismatch: %d", got[0].Seq)
	}
}

func TestRouter_Apply_5xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	reg := NewDefaultRouteRegistry()
	reg.Register(&WebhookRoute{EndpointID: "ep-3", URL: srv.URL, Method: http.MethodPost})

	sink := &captureSink{}
	r := NewWebhookRouter(eventlogtest.NewMemoryLog(), reg, Options{DLT: sink})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	env := newTestEnvelope(t, "ep-3")
	err := r.Apply(ctx, nil, env)
	if err == nil {
		t.Fatal("Apply 5xx should return error so runner retries")
	}
	if got := sink.Snapshot(); len(got) != 0 {
		t.Fatalf("no DLT expected on transient 5xx, got %d", len(got))
	}
}

type denyAllSSRF struct{}

func (denyAllSSRF) Check(string) error { return errSSRFDeny }

var errSSRFDeny = errMsg("ssrf_blocked")

type errMsg string

func (e errMsg) Error() string { return string(e) }

func TestRouter_Apply_SSRFBlockedWritesDLT(t *testing.T) {
	reg := NewDefaultRouteRegistry()
	reg.Register(&WebhookRoute{EndpointID: "ep-4", URL: "http://internal.host/x"})

	sink := &captureSink{}
	r := NewWebhookRouter(eventlogtest.NewMemoryLog(), reg, Options{
		SSRF: denyAllSSRF{},
		DLT:  sink,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	env := newTestEnvelope(t, "ep-4")
	if err := r.Apply(ctx, nil, env); err != nil {
		t.Fatalf("ssrf-blocked Apply should ack with no error, got: %v", err)
	}
	got := sink.Snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 DLT entry on SSRF block, got %d", len(got))
	}
}

func TestRouter_Apply_NoRouteIsAck(t *testing.T) {
	reg := NewDefaultRouteRegistry()
	sink := &captureSink{}
	r := NewWebhookRouter(eventlogtest.NewMemoryLog(), reg, Options{DLT: sink})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	env := newTestEnvelope(t, "ep-unknown")
	if err := r.Apply(ctx, nil, env); err != nil {
		t.Fatalf("no-route Apply should ack, got: %v", err)
	}
	if got := sink.Snapshot(); len(got) != 0 {
		t.Fatalf("no DLT expected when no route is registered, got %d", len(got))
	}
}
