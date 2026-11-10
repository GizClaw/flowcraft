package message_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// Metadata is part of the checkpointed message, so it has to round-trip and it
// has to survive cloning: a stored message must not change when a caller edits
// the copy it holds.
func TestMessageMetadataRoundTripsAndClones(t *testing.T) {
	original := message.Message{
		Role:    message.RoleUser,
		Content: message.NewTextContent("hi"),
		Metadata: &message.Metadata{
			ReferenceTasks: []string{"task-1"},
			Extensions:     []string{"https://example.test/ext"},
			Attributes: map[string]json.RawMessage{
				"note":   json.RawMessage(`{"a":1}`),
				"part.0": json.RawMessage(`{"alt":"x"}`),
			},
		},
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded message.Message
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(original, decoded) {
		t.Fatalf("round trip changed the message:\nwant %+v\ngot  %+v", original, decoded)
	}

	cloned := original.Clone()
	if cloned.Metadata == original.Metadata {
		t.Fatal("the clone shares the metadata pointer")
	}
	cloned.Metadata.Attributes["note"][0] = 'z'
	if bytes.Equal(original.Metadata.Attributes["note"], cloned.Metadata.Attributes["note"]) {
		t.Fatal("editing the clone changed the original's metadata")
	}
	if cloned.Metadata.Attributes["part.0"][0] == 'z' {
		t.Fatal("the clone's part attribute changed with another key")
	}
}

// A message without metadata must not grow a key on the wire: old checkpoints
// and new ones stay compatible.
func TestMessageWithoutMetadataOmitsTheKey(t *testing.T) {
	raw, err := json.Marshal(message.NewTextMessage(message.RoleUser, "hi"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "metadata") {
		t.Fatalf("message = %s, want no metadata key", raw)
	}
}
