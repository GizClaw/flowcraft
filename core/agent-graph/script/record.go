package script

import (
	"encoding/json"

	"github.com/GizClaw/flowcraft/core/agent"
)

// The record kinds one script invocation produces.
//
// A record is the durable fact a step wrote: it is persisted with the step's
// checkpoint, which is what lets a replay answer a tool call instead of running it
// again, and what lets a pinned node name the version it ran under.
const (
	// RecordRun names one invocation: which source, and which version of it.
	RecordRun = "script.run"
	// RecordTool names one tool call a script made.
	RecordTool = "script.tool"
)

// RunRecord is the payload of [RecordRun].
type RunRecord struct {
	// Ref is the source reference as the node config wrote it.
	Ref string `json:"ref"`
	// Version is the source version the invocation compiled.
	Version string `json:"version"`
}

// ToolRecord is the payload of [RecordTool].
type ToolRecord struct {
	// Name is the tool the script asked for.
	Name string `json:"name"`
	// ArgumentsHash is the canonical hash of the call's arguments: the trace keeps
	// the identity, not the payload, and the same hash is how a replay finds the
	// record again.
	ArgumentsHash string `json:"arguments_hash"`
	// Index is the call's position in the invocation, so two identical calls in
	// one script stay distinguishable.
	Index int `json:"index"`
	// OK reports whether the tool succeeded.
	OK bool `json:"ok"`
	// Text is what the tool answered. It is recorded because a replay has to
	// answer with the same bytes.
	Text string `json:"text,omitempty"`
}

// payload encodes one record payload. A payload that cannot encode is a bug in
// this package, not a caller error, so it is dropped rather than failing a run
// that already did the work.
func payload(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// runRecord is the fact every invocation records.
func runRecord(ref, version string) agent.Record {
	return agent.Record{Kind: RecordRun, Payload: payload(RunRecord{Ref: ref, Version: version})}
}
