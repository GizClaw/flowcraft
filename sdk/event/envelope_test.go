package event

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestNewEnvelope_StructPayload(t *testing.T) {
	ctx := context.Background()
	type ping struct {
		N int    `json:"n"`
		S string `json:"s"`
	}
	env, err := NewEnvelope(ctx, "demo.subject", ping{N: 7, S: "x"})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env.ID == "" {
		t.Fatal("ID not populated")
	}
	if env.Time.IsZero() {
		t.Fatal("Time not populated")
	}
	var got ping
	if err := env.Decode(&got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.N != 7 || got.S != "x" {
		t.Fatalf("decoded payload mismatch: %+v", got)
	}
}

func TestNewEnvelope_RawMessageReused(t *testing.T) {
	ctx := context.Background()
	raw := json.RawMessage(`{"already":"encoded"}`)
	env, err := NewEnvelope(ctx, "demo.raw", raw)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if string(env.Payload) != string(raw) {
		t.Fatalf("payload should be reused verbatim, got %s", env.Payload)
	}
}

func TestNewEnvelope_NilPayload(t *testing.T) {
	env, err := NewEnvelope(context.Background(), "demo.empty", nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env.Payload != nil {
		t.Fatalf("nil payload should stay nil, got %s", env.Payload)
	}
	// Decode on nil payload is a no-op.
	var dst map[string]any
	if err := env.Decode(&dst); err != nil {
		t.Fatalf("Decode nil: %v", err)
	}
}

func TestNewEnvelope_RejectsInvalidSubject(t *testing.T) {
	_, err := NewEnvelope(context.Background(), "", "x")
	if !errors.Is(err, ErrInvalidSubject) {
		t.Fatalf("want ErrInvalidSubject, got %v", err)
	}
}

func TestEnvelope_HeadersHelpers(t *testing.T) {
	var env Envelope
	env.SetRunID("r1")
	env.SetNodeID("n1")
	env.SetActorID("a1")
	env.SetGraphID("g1")
	env.SetTenant("t1")

	if env.RunID() != "r1" || env.NodeID() != "n1" || env.ActorID() != "a1" || env.GraphID() != "g1" || env.Tenant() != "t1" {
		t.Fatalf("typed accessors disagree: %+v", env.Headers)
	}
	// Zero envelope returns "" without panic.
	var zero Envelope
	if zero.RunID() != "" || zero.NodeID() != "" {
		t.Fatal("zero envelope should return empty strings")
	}
}

func TestEnvelope_RoundtripJSON(t *testing.T) {
	env, err := NewEnvelope(context.Background(), "round.trip", map[string]int{"n": 1})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	env.SetRunID("r1")
	env.Source = "test"

	buf, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Envelope
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Subject != env.Subject || out.RunID() != "r1" || out.Source != "test" {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
}

func TestMustEnvelope_PanicsOnBadSubject(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on invalid subject")
		}
	}()
	_ = MustEnvelope(context.Background(), "", nil)
}
