package event

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// TestNewEnvelope_ByteSliceReused asserts the documented fast-path:
// passing a []byte payload skips the JSON encode and is reused
// verbatim. Callers rely on this for zero-copy republishing of
// already-encoded blobs (e.g. SSE re-broadcast).
func TestNewEnvelope_ByteSliceReused(t *testing.T) {
	raw := []byte(`{"k":"v"}`)
	env, err := NewEnvelope(context.Background(), "demo.bytes", raw)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if string(env.Payload) != string(raw) {
		t.Fatalf("[]byte payload should be reused verbatim, got %s", env.Payload)
	}
}

// TestNewEnvelope_NilByteSlice and _NilRawMessage cover the explicit
// nil-guard branches inside NewEnvelope so a typed-nil payload does
// not slip past as an empty-but-non-nil RawMessage.
func TestNewEnvelope_NilByteSlice(t *testing.T) {
	var raw []byte
	env, err := NewEnvelope(context.Background(), "demo.nil-bytes", raw)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env.Payload != nil {
		t.Fatalf("nil []byte should leave Payload nil, got %s", env.Payload)
	}
}

func TestNewEnvelope_NilRawMessage(t *testing.T) {
	var raw json.RawMessage
	env, err := NewEnvelope(context.Background(), "demo.nil-raw", raw)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env.Payload != nil {
		t.Fatalf("nil json.RawMessage should leave Payload nil, got %s", env.Payload)
	}
}

// TestNewEnvelope_MarshalFailure forces the json.Marshal error branch
// by passing a value json.Marshal cannot encode (a channel). This
// branch is what guarantees malformed payloads surface synchronously
// rather than later at decode time on the consumer side.
func TestNewEnvelope_MarshalFailure(t *testing.T) {
	bad := make(chan int)
	_, err := NewEnvelope(context.Background(), "demo.bad", bad)
	if err == nil {
		t.Fatal("expected marshal error for chan payload, got nil")
	}
	if !strings.Contains(err.Error(), "marshal payload") {
		t.Fatalf("error %q should mention marshal payload", err)
	}
}

// TestEnvelope_DecodeError confirms Decode wraps the underlying
// json.Unmarshal failure with subject context, so consumers can tell
// which subject misbehaved when many envelopes share a sink.
func TestEnvelope_DecodeError(t *testing.T) {
	env := Envelope{Subject: "demo.bad", Payload: json.RawMessage(`{not json}`)}
	var dst map[string]any
	err := env.Decode(&dst)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "demo.bad") {
		t.Fatalf("error %q should mention subject", err)
	}
}

// TestEnvelope_HeaderOnZero pins the zero-value behaviour: Header(k)
// on an Envelope with a nil Headers map must return "" without
// panicking. Callers depend on this for header-optional events.
func TestEnvelope_HeaderOnZero(t *testing.T) {
	var env Envelope
	if got := env.Header("missing"); got != "" {
		t.Fatalf("Header on zero envelope = %q, want \"\"", got)
	}
}
