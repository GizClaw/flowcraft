package component

import (
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TestShardForAddressAgreesWithValidate keeps the two switches that group
// kinds from drifting apart: every kind the address validation accepts has
// exactly one shard, the kind it rejects (profile) has none, and the shard
// carries the id the kind is addressed by.
func TestShardForAddressAgreesWithValidate(t *testing.T) {
	kinds := []corememory.ContextItemKind{
		corememory.ContextRawMessage,
		corememory.ContextFact,
		corememory.ContextSummary,
		corememory.ContextProfile,
		corememory.ContextDocumentResource,
		corememory.ContextDocumentSection,
		corememory.ContextDocumentChunk,
		corememory.ContextDocumentSummary,
	}
	for _, kind := range kinds {
		address := CandidateAddress{
			Kind: kind, ConversationID: "c1", DatasetID: "d1", DocumentID: "doc", ItemID: "item",
		}
		validated := address.Validate() == nil
		shard, err := ShardForAddress(address)
		if validated != (err == nil) {
			t.Fatalf("%s: Validate err = %v, ShardForAddress err = %v", kind, address.Validate(), err)
		}
		if err != nil {
			continue
		}
		switch kind {
		case corememory.ContextRawMessage, corememory.ContextFact, corememory.ContextSummary:
			if want, _ := ConversationShard("c1"); shard != want {
				t.Fatalf("%s lives in %q, want %q", kind, shard, want)
			}
		default:
			if want, _ := DatasetShard("d1"); shard != want {
				t.Fatalf("%s lives in %q, want %q", kind, shard, want)
			}
		}
	}
	for _, address := range []CandidateAddress{
		{},
		{Kind: corememory.ContextProfile, ConversationID: "c1", ItemID: "profile"},
		{Kind: corememory.ContextFact},
		{Kind: corememory.ContextDocumentChunk, DocumentID: "doc"},
		{Kind: "unknown", ConversationID: "c1"},
	} {
		if _, err := ShardForAddress(address); err == nil {
			t.Errorf("ShardForAddress(%+v) named a shard for an address without one", address)
		}
	}
}
