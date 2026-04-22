package eventlog

import (
	"bytes"
	"encoding/json"
	"time"
)

// Actor is the on-wire envelope.actor field; mirrors contracts/payloads/actor.yaml
// (id / kind / realm_id). Authorization-time data such as scopes/roles/runtime_id
// belongs to policy.Actor (a separate type) and must not leak into the audit
// envelope.
type Actor struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	RealmID string `json:"realm_id,omitempty"`
}

// Envelope is the on-wire event frame stored in the append-only log.
// Codegen (publish_gen.go) fills Partition/Type/Version/Category/Payload;
// the runtime sets Seq and Ts at append time.
//
// The wire format is the single source of truth across WS / SSE / HTTP pull;
// any field added here must also appear in contracts/events.yaml envelope.fields
// and pass `make events-check`.
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

// PartitionKind represents one of the registered partition prefixes
// (the second column of contracts/events.yaml#partitions). Routing layers
// import these constants instead of typing the bare strings inline.
type PartitionKind string

const (
	PartitionKindRuntime         PartitionKind = "runtime"
	PartitionKindCard            PartitionKind = "card"
	PartitionKindWebhookEndpoint PartitionKind = "webhook"
	PartitionKindCronRule        PartitionKind = "cron"
	PartitionKindRealm           PartitionKind = "realm"
	PartitionKindActor           PartitionKind = "actor"
)

// AllPartitionKinds returns every registered prefix in declaration order;
// caller must not mutate the returned slice.
func AllPartitionKinds() []PartitionKind {
	return []PartitionKind{
		PartitionKindRuntime,
		PartitionKindCard,
		PartitionKindWebhookEndpoint,
		PartitionKindCronRule,
		PartitionKindRealm,
		PartitionKindActor,
	}
}

// MarshalEnvelope is the canonical serializer used by every transport
// (wshub, ssehub, HTTP pull). All three MUST go through this function so
// downstream consumers can byte-compare envelopes regardless of channel.
//
// We deliberately disable HTML escaping (default true in encoding/json) and
// strip the trailing newline encoding/Encode adds, so the output is the
// shortest UTF-8 JSON encoding of env.
func MarshalEnvelope(env Envelope) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

// SplitPartition splits "kind:id" into its components. ok=false when the
// string does not match any registered prefix.
func SplitPartition(p string) (kind PartitionKind, id string, ok bool) {
	for _, k := range AllPartitionKinds() {
		prefix := string(k) + ":"
		if len(p) > len(prefix) && p[:len(prefix)] == prefix {
			return k, p[len(prefix):], true
		}
	}
	return "", "", false
}
