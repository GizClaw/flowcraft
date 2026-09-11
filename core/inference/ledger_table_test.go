package inference

import (
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// TestGenerateLedgerCoversPartKinds is the guard that keeps the generate ledger
// in step with the content vocabulary: adding a part kind to core/message fails
// here until both positions say what that kind activates. Without it a new kind
// would simply never enter the ledger — invisible to the driver, so never
// compiled, never reported.
func TestGenerateLedgerCoversPartKinds(t *testing.T) {
	for _, kind := range message.PartKinds() {
		if _, ok := generateContextPartFields[kind]; !ok {
			t.Errorf("generateContextPartFields has no row for %q", kind)
		}
		if _, ok := generateInputPartFields[kind]; !ok {
			t.Errorf("generateInputPartFields has no row for %q", kind)
		}
	}
	for kind := range generateContextPartFields {
		if err := kind.Validate(); err != nil {
			t.Errorf("context table has a row for a non-kind %q", kind)
		}
	}
	for kind := range generateInputPartFields {
		if err := kind.Validate(); err != nil {
			t.Errorf("input table has a row for a non-kind %q", kind)
		}
	}
}

// TestPartFieldLookupsMatchTheTables pins the exported lookups against the
// tables they read, including which kinds have no row: context and input cover
// the whole vocabulary, embed excludes reasoning.
func TestPartFieldLookupsMatchTheTables(t *testing.T) {
	for _, kind := range message.PartKinds() {
		field, ok := GenerateContextPartField(kind)
		if want, exists := generateContextPartFields[kind]; !ok || field != want || !exists {
			t.Errorf("GenerateContextPartField(%q) = (%q, %v)", kind, field, ok)
		}
		field, ok = GenerateInputPartField(kind)
		if want, exists := generateInputPartFields[kind]; !ok || field != want || !exists {
			t.Errorf("GenerateInputPartField(%q) = (%q, %v)", kind, field, ok)
		}
		field, ok = EmbedItemPartField(kind)
		want, exists := embedItemPartFields[kind]
		if kind == message.PartReasoning {
			if ok || field != "" {
				t.Errorf("EmbedItemPartField(%q) = (%q, %v), want no row", kind, field, ok)
			}
			continue
		}
		if !ok || field != want || !exists {
			t.Errorf("EmbedItemPartField(%q) = (%q, %v)", kind, field, ok)
		}
	}
	// A kind outside the vocabulary is vocabulary drift, reported as no row
	// rather than as an empty field.
	for _, lookup := range []func(message.PartKind) (FieldID, bool){
		GenerateContextPartField, GenerateInputPartField, EmbedItemPartField,
	} {
		if field, ok := lookup(message.PartKind("not-a-kind")); ok || field != "" {
			t.Errorf("lookup of an unknown kind = (%q, %v), want no row", field, ok)
		}
	}
}

// TestEmbedLedgerCoversPartKinds pins the same rule for embeddings, with the
// one documented exclusion: reasoning is not an input an embedding call can
// consume, so it has no field.
func TestEmbedLedgerCoversPartKinds(t *testing.T) {
	for _, kind := range message.PartKinds() {
		_, ok := embedItemPartFields[kind]
		if kind == message.PartReasoning {
			if ok {
				t.Errorf("reasoning unexpectedly has an embed field")
			}
			continue
		}
		if !ok {
			t.Errorf("embedItemPartFields has no row for %q", kind)
		}
	}
}

// TestPartFieldOrderFollowsVocabulary pins that a request carrying every kind
// produces the tables' fields in vocabulary order, so a request's ledger is
// deterministic regardless of the order its parts were assembled in.
func TestPartFieldOrderFollowsVocabulary(t *testing.T) {
	parts := make([]message.Part, 0, len(message.PartKinds()))
	// Reverse order, so a table that followed the request would be caught.
	for _, kind := range slices.Backward(message.PartKinds()) {
		parts = append(parts, partForKind(t, kind))
	}
	request := GenerateRequest{Input: GenerateInput{
		Role: InputRoleUser,
		Content: InputContent{
			Content: message.Content{Parts: parts},
			Intent:  Intent{Text: &TextIntent{}},
		},
	}}
	fields := request.ActiveFields()
	got := make([]FieldID, 0, len(message.PartKinds()))
	for _, field := range fields {
		if _, ok := generateInputPartFieldsByField(field); ok {
			got = append(got, field)
		}
	}
	want := make([]FieldID, 0, len(message.PartKinds()))
	for _, kind := range message.PartKinds() {
		want = append(want, generateInputPartFields[kind])
	}
	if !slices.Equal(got, want) {
		t.Fatalf("input part fields = %v, want %v", got, want)
	}
}

// generateInputPartFieldsByField reports whether field is one of the input part
// fields, letting the order test pick them out of the full ledger.
func generateInputPartFieldsByField(field FieldID) (message.PartKind, bool) {
	for kind, candidate := range generateInputPartFields {
		if candidate == field {
			return kind, true
		}
	}
	return "", false
}

// partForKind returns the zero value of one kind's part struct. The ledger only
// reads Kind(), so validity is irrelevant here and the fixture stays trivial.
func partForKind(t *testing.T, kind message.PartKind) message.Part {
	t.Helper()
	switch kind {
	case message.PartText:
		return message.TextPart{}
	case message.PartImage:
		return message.ImagePart{}
	case message.PartAudio:
		return message.AudioPart{}
	case message.PartVideo:
		return message.VideoPart{}
	case message.PartFile:
		return message.FilePart{}
	case message.PartData:
		return message.DataPart{}
	case message.PartToolCall:
		return message.ToolCallPart{}
	case message.PartToolResult:
		return message.ToolResultPart{}
	case message.PartReasoning:
		return message.ReasoningPart{}
	default:
		t.Fatalf("no fixture for part kind %q", kind)
		return nil
	}
}
