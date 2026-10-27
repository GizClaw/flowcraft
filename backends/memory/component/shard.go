package component

import (
	"errors"
	"fmt"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// A shard is the smallest candidate set a read has to touch: one
// conversation's chat records, or one dataset's documents. It is a storage
// concept that sits below Scope, which stays the hard partition, and it is
// derived from the address a record already carries -- there is no id-to-shard
// catalog, and no catch-all shard for records whose address cannot be
// computed. Those are rejected, so a write that cannot say where it belongs
// fails instead of landing in a shard no selector can ever name.
const (
	// ShardConversationPrefix marks a shard that holds one conversation's chat
	// records: raw_message, fact and summary.
	ShardConversationPrefix = "c:"
	// ShardDatasetPrefix marks a shard that holds one dataset's documents: the
	// four document kinds.
	ShardDatasetPrefix = "d:"
)

// ConversationShard returns the shard holding one conversation's chat records.
func ConversationShard(conversationID string) (string, error) {
	if conversationID == "" {
		return "", errors.New("memory component: conversation shard needs a conversation_id")
	}
	return ShardConversationPrefix + conversationID, nil
}

// DatasetShard returns the shard holding one dataset's documents.
func DatasetShard(datasetID string) (string, error) {
	if datasetID == "" {
		return "", errors.New("memory component: dataset shard needs a dataset_id")
	}
	return ShardDatasetPrefix + datasetID, nil
}

// ShardForAddress names the shard one candidate address belongs to. The kind
// groups here are exactly the groups CandidateAddress.Validate accepts, and a
// test keeps them in step: a kind the address validation rejects (profile) has
// no shard, and a kind it accepts has exactly one.
func ShardForAddress(address CandidateAddress) (string, error) {
	switch address.Kind {
	case corememory.ContextRawMessage, corememory.ContextFact, corememory.ContextSummary:
		return ConversationShard(address.ConversationID)
	case corememory.ContextDocumentResource, corememory.ContextDocumentSection,
		corememory.ContextDocumentChunk, corememory.ContextDocumentSummary:
		return DatasetShard(address.DatasetID)
	default:
		return "", fmt.Errorf("memory component: unsupported candidate address kind %q", address.Kind)
	}
}
