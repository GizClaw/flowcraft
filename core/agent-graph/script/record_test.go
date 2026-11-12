package script

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
)

// payloadOf is the encoder this package uses, so a test asserts against the same
// bytes a record holds.
func payloadOf(value any) json.RawMessage {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}

// A reader outside this package renders a script's facts through this function,
// so both kinds must come back as the type that wrote them.
func TestDecodeRecordOwnsItsKinds(t *testing.T) {
	cases := []struct {
		kind  string
		value any
	}{
		{RecordRun, RunRecord{Ref: "rules/vote.star", Version: "sha256:v", Replayed: true}},
		{RecordTool, ToolRecord{Name: "read", ArgumentsHash: "sha256:a", Index: 2, OK: true, Text: "done"}},
	}
	for _, tc := range cases {
		payload := payloadOf(tc.value)
		decoded, ok := DecodeRecord(agent.EventRecord{Kind: tc.kind, Payload: payload})
		if !ok {
			t.Errorf("%s: not decoded", tc.kind)
			continue
		}
		if again := payloadOf(decoded); string(again) != string(payload) {
			t.Errorf("%s round-tripped to a different value:\n in: %s\nout: %s", tc.kind, payload, again)
		}
	}
}

// A kind this node does not own is left to the next decoder.
func TestDecodeRecordIgnoresForeignKinds(t *testing.T) {
	for _, kind := range []string{"graph.state", "llm.call", "message", ""} {
		if _, ok := DecodeRecord(agent.EventRecord{Kind: kind, Payload: payloadOf(ToolRecord{})}); ok {
			t.Errorf("kind %q was claimed by the script decoder", kind)
		}
	}
}
