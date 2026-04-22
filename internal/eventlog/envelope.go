package eventlog

import (
	"encoding/json"
	"time"
)

// Actor is the optional envelope.actor field (see contracts/payloads/actor.yaml).
type Actor struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	RealmID string `json:"realm_id"`
}

// Envelope is the on-wire event frame stored in the append-only log.
// Codegen (publish_gen.go) fills Partition/Type/Version/Category/Payload;
// the runtime sets Seq and Ts at append time.
type Envelope struct {
	Seq       int64           `json:"seq"`
	Partition string          `json:"partition"`
	Type      string          `json:"type"`
	Version   int             `json:"version"`
	Category  string          `json:"category"`
	Ts        string          `json:"ts"`
	Payload   json.RawMessage `json:"payload"`
	Actor     *Actor          `json:"actor,omitempty"`
	TraceID   string          `json:"trace_id,omitempty"`
	SpanID    string          `json:"span_id,omitempty"`
}

// NowRFC3339Nano returns the current UTC time in RFC3339Nano, used by publishers.
func NowRFC3339Nano() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
