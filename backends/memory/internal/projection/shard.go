package projection

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

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

// Plan returns the shards this selector can read from, in a stable order: the
// conversation shard first when chat is selectable, then the dataset shards
// sorted.
func (selector Selector) Plan() (ShardPlan, error) {
	if selector.Global() {
		return ShardPlan{All: true}, nil
	}
	var plan ShardPlan
	if selector.conversation != "" {
		shard, err := component.ConversationShard(selector.conversation)
		if err != nil {
			return ShardPlan{}, err
		}
		plan.Shards = append(plan.Shards, shard)
	}
	for _, datasetID := range selector.Datasets() {
		shard, err := component.DatasetShard(datasetID)
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
	var kind string
	switch {
	case strings.HasPrefix(segment, "c_"):
		kind = "c"
	case strings.HasPrefix(segment, "d_"):
		kind = "d"
	default:
		return "", fmt.Errorf("projection: %q is not a shard segment", segment)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(segment[2:])
	if err != nil {
		return "", fmt.Errorf("projection: decode shard segment %q: %w", segment, err)
	}
	if kind == "c" {
		return component.ConversationShard(string(decoded))
	}
	return component.DatasetShard(string(decoded))
}

func splitShard(shard string) (string, string, error) {
	switch {
	case strings.HasPrefix(shard, component.ShardConversationPrefix):
		id := strings.TrimPrefix(shard, component.ShardConversationPrefix)
		if id == "" {
			return "", "", errors.New("projection: conversation shard needs a conversation_id")
		}
		return "c_", id, nil
	case strings.HasPrefix(shard, component.ShardDatasetPrefix):
		id := strings.TrimPrefix(shard, component.ShardDatasetPrefix)
		if id == "" {
			return "", "", errors.New("projection: dataset shard needs a dataset_id")
		}
		return "d_", id, nil
	default:
		return "", "", fmt.Errorf("projection: %q is not a shard", shard)
	}
}
