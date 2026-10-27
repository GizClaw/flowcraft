package projection

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// A shard is the smallest candidate set a read has to touch: one
// conversation's chat records, or one dataset's documents. It is a storage
// concept that sits below Scope, which stays the hard partition, and it is
// derived from the address a write already carries — there is no id-to-shard
// catalog, and no catch-all shard for records whose address cannot be
// computed. Those are rejected, so a write failure is visible instead of
// landing in a shard no selector can ever name.
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
		return "", errors.New("projection: conversation shard needs a conversation_id")
	}
	return ShardConversationPrefix + conversationID, nil
}

// DatasetShard returns the shard holding one dataset's documents.
func DatasetShard(datasetID string) (string, error) {
	if datasetID == "" {
		return "", errors.New("projection: dataset shard needs a dataset_id")
	}
	return ShardDatasetPrefix + datasetID, nil
}

// ShardForAddress names the shard one candidate address belongs to. The kind
// groups here are exactly the groups CandidateAddress.Validate accepts, and
// the test that keeps them in step fails if either side learns a new kind
// alone: a kind the address validation rejects (profile) has no shard, and a
// kind it accepts must have one.
func ShardForAddress(address component.CandidateAddress) (string, error) {
	switch address.Kind {
	case corememory.ContextRawMessage, corememory.ContextFact, corememory.ContextSummary:
		return ConversationShard(address.ConversationID)
	case corememory.ContextDocumentResource, corememory.ContextDocumentSection,
		corememory.ContextDocumentChunk, corememory.ContextDocumentSummary:
		return DatasetShard(address.DatasetID)
	default:
		return "", fmt.Errorf("projection: unsupported candidate address kind %q", address.Kind)
	}
}

// ShardPlan is the set of shards one request can read from. All means the
// request narrows nothing: the plan is every shard in the scope, and a reader
// enumerates them instead of taking the list from here.
//
// The plan is exact, not merely safe: every shard it names can hold matching
// records, so scanning the plan and filtering by the same selector returns the
// same records as scanning the whole scope. That is the property the routing
// test pins, and it is what lets the lanes skip shards without changing a
// result.
type ShardPlan struct {
	All    bool
	Shards []string
}

// Empty reports whether the plan can match nothing at all — a request that
// names the document axis with an empty list.
func (plan ShardPlan) Empty() bool { return !plan.All && len(plan.Shards) == 0 }

// Holds reports whether one shard is inside the plan.
func (plan ShardPlan) Holds(shard string) bool {
	if plan.All {
		return true
	}
	for _, planned := range plan.Shards {
		if planned == shard {
			return true
		}
	}
	return false
}

// Plan returns the shards this selector can read from, in a stable order:
// the conversation shard first when chat is selectable, then the dataset
// shards sorted.
func (selector Selector) Plan() (ShardPlan, error) {
	if selector.Global() {
		return ShardPlan{All: true}, nil
	}
	var plan ShardPlan
	if selector.conversation != "" {
		shard, err := ConversationShard(selector.conversation)
		if err != nil {
			return ShardPlan{}, err
		}
		plan.Shards = append(plan.Shards, shard)
	}
	for _, datasetID := range selector.Datasets() {
		shard, err := DatasetShard(datasetID)
		if err != nil {
			return ShardPlan{}, err
		}
		plan.Shards = append(plan.Shards, shard)
	}
	return plan, nil
}

// ShardsFor compiles request metadata and returns the shards it can read
// from. Lanes plan a read with it; the store enumerates a scope's shards for
// the All case.
func ShardsFor(metadata corememory.Metadata) (ShardPlan, error) {
	selector, err := ParseSelector(metadata)
	if err != nil {
		return ShardPlan{}, err
	}
	return selector.Plan()
}

// ShardSegment returns the single path segment a shard occupies under
// indexes/<name>/shards/. The id is encoded like every other name (base64url
// without padding) so that separators, NUL and non-ASCII bytes in a
// conversation or dataset id cannot become path structure; the c_/d_ prefix
// keeps the two address families apart after encoding.
func ShardSegment(shard string) (string, error) {
	prefix, id, err := splitShard(shard)
	if err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString([]byte(id)), nil
}

// ParseShardSegment is the inverse of ShardSegment, and rejects anything that
// is not one: the storage scan uses it to tell a shard directory from a
// sibling it does not own.
func ParseShardSegment(segment string) (string, error) {
	var prefix string
	switch {
	case strings.HasPrefix(segment, "c_"):
		prefix = ShardConversationPrefix
	case strings.HasPrefix(segment, "d_"):
		prefix = ShardDatasetPrefix
	default:
		return "", fmt.Errorf("projection: %q is not a shard segment", segment)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(segment[2:])
	if err != nil {
		return "", fmt.Errorf("projection: decode shard segment %q: %w", segment, err)
	}
	if prefix == ShardConversationPrefix {
		return ConversationShard(string(decoded))
	}
	return DatasetShard(string(decoded))
}

func splitShard(shard string) (string, string, error) {
	switch {
	case strings.HasPrefix(shard, ShardConversationPrefix):
		id := strings.TrimPrefix(shard, ShardConversationPrefix)
		if id == "" {
			return "", "", errors.New("projection: conversation shard needs a conversation_id")
		}
		return "c_", id, nil
	case strings.HasPrefix(shard, ShardDatasetPrefix):
		id := strings.TrimPrefix(shard, ShardDatasetPrefix)
		if id == "" {
			return "", "", errors.New("projection: dataset shard needs a dataset_id")
		}
		return "d_", id, nil
	default:
		return "", "", fmt.Errorf("projection: %q is not a shard", shard)
	}
}
