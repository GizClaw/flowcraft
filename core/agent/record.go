package agent

import (
	"encoding/json"
	"time"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// Record is one durable fact an engine writes beside its state: a model call,
// a side effect, an input digest. Kind is the engine's vocabulary; a store only
// persists the bytes.
type Record struct {
	Kind    string
	Payload json.RawMessage
	// At is when the fact happened, as the producing node knows it. Zero asks the
	// engine to stamp the save: a node that records "this happened" without a
	// better answer still gets the instant it was written. The per-kind anchor is
	// documented on [EventRecord.At].
	At time.Time
}

// MessageDigest is the identity of one message that entered a prompt.
//
// Origin is the provenance tag the producing node attached
// (message.Metadata.Attributes["origin"]): "user", "assistant",
// "tool:<name>", "retrieved:<id>" or "system". It is what turns "the context
// changed" into "this retrieved chunk changed".
type MessageDigest struct {
	Role   message.Role `json:"role"`
	Origin string       `json:"origin,omitempty"`
	Chars  int          `json:"chars"`
	Hash   string       `json:"hash"`
}

// PromptSegment is a roll-up dimension over digests, for dashboards and export.
// It is not a storage key: attribution points at a digest.
type PromptSegment struct {
	ID    string `json:"id"`
	Chars int    `json:"chars"`
	Hash  string `json:"hash"`
}

// EffectStatus is where a side effect stands.
type EffectStatus string

const (
	// EffectStarted is written before the effect runs, so a crash leaves a
	// trace of an action whose outcome nobody recorded.
	EffectStarted EffectStatus = "started"
	// EffectDone is written after the effect returned successfully.
	EffectDone EffectStatus = "done"
	// EffectFailed is written after the effect returned an error.
	EffectFailed EffectStatus = "failed"
)

// EffectRecord is one tool execution as the trace keeps it.
//
// IdempotencyKey lets the next attempt recognise an effect that already
// happened; Status started without a matching done/failed is what makes a
// resume refuse to guess.
type EffectRecord struct {
	CallID         string       `json:"call_id"`
	ToolName       string       `json:"tool_name"`
	ArgsHash       string       `json:"args_hash,omitempty"`
	IdempotencyKey string       `json:"idempotency_key,omitempty"`
	Status         EffectStatus `json:"status"`
	ResultHash     string       `json:"result_hash,omitempty"`
	ErrorKind      string       `json:"error_kind,omitempty"`
	DurationMs     int64        `json:"duration_ms,omitempty"`
}

// LLMCallRecord is one model call as the trace keeps it.
//
// Response is the assembled assistant turn; the engine stores it as a blob and
// replaces it with ResponseRef, so the event stays small. RequestHash is the
// key a replay looks the call up by.
type LLMCallRecord struct {
	RequestHash string          `json:"request_hash,omitempty"`
	Model       model.ModelRef  `json:"model,omitempty"`
	Usage       usage.Usage     `json:"usage,omitempty"`
	StopReason  string          `json:"stop_reason,omitempty"`
	LatencyMs   int64           `json:"latency_ms,omitempty"`
	ErrorKind   string          `json:"error_kind,omitempty"`
	Response    []byte          `json:"response,omitempty"`
	ResponseRef string          `json:"response_ref,omitempty"`
	Messages    []MessageDigest `json:"messages,omitempty"`
	Segments    []PromptSegment `json:"segments,omitempty"`
}
