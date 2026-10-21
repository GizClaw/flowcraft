package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft/plugin/protocol"
)

// recordingEvents is the application primitive of a test host: it records
// what a plugin published, so a test can tell a granted call from a refused
// one.
type recordingEvents struct {
	subjects []string
	payloads []any
}

func (e *recordingEvents) emit(_ context.Context, call Call) (any, error) {
	subject, err := call.String("subject")
	if err != nil {
		return nil, err
	}
	e.subjects = append(e.subjects, subject)
	e.payloads = append(e.payloads, call.Params["payload"])
	return map[string]any{}, nil
}

// primitives is the one primitive the host tests serve: an emit that needs
// a grant, which is the half of the catalog they are about.
func (e *recordingEvents) primitives() []Primitive {
	return []Primitive{{
		Resource: "events", Op: "emit", Grant: "events:emit",
		Description: "Publish one host event",
		Params:      json.RawMessage(`{"type":"object","properties":{"subject":{"type":"string"},"payload":{}},"required":["subject"]}`),
		Handler:     e.emit,
	}}
}

// hostProbe is a plugin whose declaration is preceded by primitive calls: it
// learns what it is denied, then tries to emit an event.
type hostProbe struct {
	about   About
	emitErr error
}

func (p *hostProbe) Declare(
	ctx context.Context,
	_ *protocol.HelloResponse,
	host protocol.HostClient,
) (*protocol.Declaration, error) {
	if err := protocol.Invoke(ctx, host, "host", "about", nil, &p.about); err != nil {
		return nil, err
	}
	p.emitErr = protocol.Invoke(ctx, host, "events", "emit", map[string]any{
		"subject": "plugin.ping",
		"payload": map[string]any{"n": 1},
	}, nil)
	return &protocol.Declaration{}, nil
}

func (p *hostProbe) Call(context.Context, *protocol.CallRequest) ([]byte, error) {
	return []byte(`{}`), nil
}

func TestHostPrimitivesAreBoundToTheConnection(t *testing.T) {
	// The connection is the identity: the same call is allowed on the
	// connection whose grants include it and refused on the one whose
	// grants do not, with nothing but the socket to tell them apart.
	for _, tc := range []struct {
		name    string
		grants  GrantSet
		allowed bool
	}{
		{name: "granted", grants: GrantSet{"events:emit": true}, allowed: true},
		{name: "denied", grants: GrantSet{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := &recordingEvents{}
			registry, err := Catalog(events.primitives(), "test", nil)
			if err != nil {
				t.Fatalf("Catalog: %v", err)
			}
			srv := newTestServer(t, ServerOptions{Registry: registry, HostVersion: "test"})
			conn := newTestConn(t, srv, ConnOptions{
				PluginID: "demo",
				Version:  "0.1.0",
				Grants:   tc.grants,
			})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			probe := &hostProbe{}
			servePlugin(t, ctx, conn, probe, protocol.ClientOptions{
				PluginID: "demo",
				Version:  "0.1.0",
			})
			if err := conn.WaitReady(ctx, 5*time.Second); err != nil {
				t.Fatalf("WaitReady: %v", err)
			}
			if tc.allowed {
				if probe.emitErr != nil {
					t.Fatalf("Emit on a granted connection: %v", probe.emitErr)
				}
				if len(events.subjects) != 1 || events.subjects[0] != "plugin.ping" {
					t.Fatalf("emitted %v, want one plugin.ping", events.subjects)
				}
				payload, _ := events.payloads[0].(map[string]any)
				if payload["n"] != float64(1) {
					t.Fatalf("payload = %v, want {\"n\":1}", events.payloads[0])
				}
			} else {
				if probe.emitErr == nil {
					t.Fatal("Emit succeeded without the events:emit grant")
				}
				// The refusal crosses the socket with its class: a
				// missing grant is a permission failure, not an
				// anonymous transport error.
				if !errdefs.IsForbidden(probe.emitErr) {
					t.Fatalf("Emit without a grant = %v, want forbidden", probe.emitErr)
				}
				if len(events.subjects) != 0 {
					t.Fatalf("emitted %v without a grant", events.subjects)
				}
			}
			denied := make(map[string]string, len(probe.about.Denied))
			for _, entry := range probe.about.Denied {
				denied[entry.Primitive] = entry.Reason
			}
			_, reported := denied["events.emit"]
			if reported == tc.allowed {
				t.Fatalf("events.emit denied=%t on a connection with granted=%t",
					reported, tc.allowed)
			}
			if !tc.allowed && denied["events.emit"] != "no_grant" {
				t.Fatalf("events.emit is denied for %q, want no_grant",
					denied["events.emit"])
			}
		})
	}
}

// TestHostCallRefusesWhatItCannotAnswer covers the three ways a generic
// entry point can be addressed wrongly. They are cheap checks on craft's
// side, and each one answers with a class rather than with silence.
func TestHostCallRefusesWhatItCannotAnswer(t *testing.T) {
	events := &recordingEvents{}
	registry, err := Catalog(events.primitives(), "test", nil)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	srv := newTestServer(t, ServerOptions{Registry: registry, HostVersion: "test"})
	conn := newTestConn(t, srv, ConnOptions{
		PluginID: "demo",
		Version:  "0.1.0",
		Grants:   GrantSet{"events:emit": true},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	probe := &hostRecorder{}
	servePlugin(t, ctx, conn, probe, protocol.ClientOptions{
		PluginID: "demo",
		Version:  "0.1.0",
	})
	if err := conn.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	if err := probe.invoke(ctx, "events", "missing", nil, nil); !errdefs.IsNotFound(err) {
		t.Fatalf("calling events.missing = %v, want not found", err)
	}
	if err := probe.invoke(ctx, "events", "Emit", nil, nil); !errdefs.IsValidation(err) {
		t.Fatalf("calling a non-token op = %v, want a validation error", err)
	}
	if err := probe.invoke(ctx, "events", "emit", []string{"subject"}, nil); !errdefs.IsValidation(err) {
		t.Fatalf("sending params that are not an object = %v, want a validation error", err)
	}
	if err := probe.invoke(ctx, "events", "emit", map[string]any{}, nil); !errdefs.IsValidation(err) {
		t.Fatalf("omitting a required param = %v, want a validation error", err)
	}
	if err := probe.invoke(ctx, "events", "emit",
		map[string]any{"subject": "plugin.ping"}, nil); err != nil {
		t.Fatalf("events.emit: %v", err)
	}
	if len(events.subjects) != 1 || events.subjects[0] != "plugin.ping" {
		t.Fatalf("emitted %v, want one plugin.ping", events.subjects)
	}
}

// hostRecorder keeps the connection's primitive surface so a test can drive
// calls after the handshake instead of only from inside Declare.
type hostRecorder struct {
	host protocol.HostClient
}

func (p *hostRecorder) Declare(
	_ context.Context,
	_ *protocol.HelloResponse,
	host protocol.HostClient,
) (*protocol.Declaration, error) {
	p.host = host
	return &protocol.Declaration{}, nil
}

func (p *hostRecorder) Call(context.Context, *protocol.CallRequest) ([]byte, error) {
	return []byte(`{}`), nil
}

func (p *hostRecorder) invoke(
	ctx context.Context,
	resource, op string,
	params, out any,
) error {
	if p.host == nil {
		return errdefs.NotAvailablef("the recorder has no host connection")
	}
	return protocol.Invoke(ctx, p.host, resource, op, params, out)
}
