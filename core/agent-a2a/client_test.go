package a2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"go.opentelemetry.io/otel/trace"
)

// peer records the headers of every request it is asked, and answers with a
// failure: these tests are about what leaves this process, not what comes back.
type peer struct {
	mu       sync.Mutex
	requests []http.Header
}

func (p *peer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.requests = append(p.requests, r.Header.Clone())
	p.mu.Unlock()
	http.Error(w, "not implemented in this test", http.StatusInternalServerError)
}

// headers returns what the peer saw, requests that carried an RPC body first.
func (p *peer) headers() []http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]http.Header(nil), p.requests...)
}

// send drives one message through the client, ignoring the peer's answer: the
// assertion is on the request.
func send(t *testing.T, ctx context.Context, cfg Config) {
	t.Helper()
	client, err := newClient(ctx, cfg)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	message := sdk.NewMessage(sdk.MessageRoleUser, sdk.NewTextPart("hi"))
	// The peer answers 500, so this returns an error by design.
	_, _ = client.SendMessage(ctx, &sdk.SendMessageRequest{Message: message})
}

// A peer is called with the caller's trace context, or the run it starts is a
// second trace nobody can join back to the delegation that caused it.
func TestClientInjectsTraceContext(t *testing.T) {
	telemetrytest.New(t)
	telemetrytest.Propagator(t)
	recorder := &peer{}
	server := httptest.NewServer(recorder)
	defer server.Close()

	parent, span := telemetry.TracerWithSuffix("test").Start(
		context.Background(), "parent")
	defer span.End()

	send(t, parent, Config{Peer: server.URL})

	headers := recorder.headers()
	if len(headers) == 0 {
		t.Fatal("the peer saw no request")
	}
	var found bool
	for _, header := range headers {
		if header.Get("traceparent") == "" {
			continue
		}
		found = true
		if got := trace.SpanContextFromContext(parent).TraceID().String(); !strings.Contains(header.Get("traceparent"), got) {
			t.Errorf("traceparent %q does not carry the caller's trace %s",
				header.Get("traceparent"), got)
		}
	}
	if !found {
		t.Fatal("no request carried a traceparent")
	}
}

// The credential and the propagation ride the same transport: wrapping one must
// not drop the other, which is exactly what a hand-built chain tends to do.
func TestClientInjectsTraceContextWithCredentials(t *testing.T) {
	telemetrytest.New(t)
	telemetrytest.Propagator(t)
	recorder := &peer{}
	server := httptest.NewServer(recorder)
	defer server.Close()

	parent, span := telemetry.TracerWithSuffix("test").Start(
		context.Background(), "parent")
	defer span.End()

	send(t, parent, Config{Peer: server.URL, Auth: []byte(`{"type":"bearer","token":"t-1"}`)})

	headers := recorder.headers()
	if len(headers) == 0 {
		t.Fatal("the peer saw no request")
	}
	for _, header := range headers {
		if header.Get("Authorization") != "Bearer t-1" {
			t.Errorf("Authorization = %q, want the bearer token", header.Get("Authorization"))
		}
		if header.Get("traceparent") == "" {
			t.Error("a request with a credential carried no traceparent")
		}
	}
}

// Without a span nothing is injected: a run that was never traced must not
// pretend to be part of a trace.
func TestClientWithoutASpanSendsNoTraceParent(t *testing.T) {
	telemetrytest.New(t)
	telemetrytest.Propagator(t)
	recorder := &peer{}
	server := httptest.NewServer(recorder)
	defer server.Close()

	send(t, context.Background(), Config{Peer: server.URL})

	for _, header := range recorder.headers() {
		if header.Get("traceparent") != "" {
			t.Errorf("an untraced call sent traceparent %q", header.Get("traceparent"))
		}
	}
}
