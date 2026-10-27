package projection

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// Selector is the compiled form of the read-side selectors the ContextProvider
// encodes into request metadata. It is the single authoritative statement of
// what a selector narrows: the lanes filter candidates with it, and the shard
// routing (ShardPlan) reads from the same instance, so a request cannot be
// routed to one set of shards and scored against another.
//
// Naming an axis narrows it, and the record's kind decides which axis judges
// it. Chat records — raw_message, fact and summary — are judged by
// conversation_id; document records — the four document kinds — are judged by
// dataset_ids. The axes are alternatives, not a conjunction:
//
//	conversation_id  dataset_ids  chat records        document records
//	unset            unset        all                 all               (global recall)
//	set              unset        this conversation   none
//	unset            set          none                these datasets
//	set              set          this conversation   these datasets
//
// The "none" cells are the point of the strict rule: a request naming one axis
// asks for that axis alone, so a conversation-scoped question no longer reads
// every document of the scope just because it left dataset_ids unset. A caller
// that wants both names both.
type Selector struct {
	conversation string
	datasets     map[string]struct{}
	// narrowDocuments records that the request named the document axis, which
	// is not the same as naming it with an empty list: "dataset_ids": "[]"
	// selects no documents, while an absent key selects all of them when the
	// conversation axis is absent too.
	narrowDocuments bool
}

// ParseSelector compiles request metadata into the selector every read path
// shares. The only key that can fail is dataset_ids, whose value is a JSON
// array written by the provider; a malformed value is an error rather than an
// empty selection, because silently reading the whole scope on a decode
// failure is the leak the strict rule exists to close.
func ParseSelector(metadata corememory.Metadata) (Selector, error) {
	selector := Selector{conversation: metadata["conversation_id"]}
	rawDatasets := metadata["dataset_ids"]
	if rawDatasets == "" {
		return selector, nil
	}
	var datasets []string
	if err := json.Unmarshal([]byte(rawDatasets), &datasets); err != nil {
		return Selector{}, fmt.Errorf("projection: decode dataset_ids selector: %w", err)
	}
	selector.narrowDocuments = true
	selector.datasets = make(map[string]struct{}, len(datasets))
	for _, datasetID := range datasets {
		selector.datasets[datasetID] = struct{}{}
	}
	return selector, nil
}

// Global reports whether the request narrows nothing, which makes it the exact
// whole-scope recall: every shard, every address, no truncation.
func (selector Selector) Global() bool {
	return selector.conversation == "" && !selector.narrowDocuments
}

// Matches reports whether one addressed record belongs to the request.
func (selector Selector) Matches(address component.CandidateAddress) bool {
	if selector.Global() {
		return true
	}
	switch address.Kind {
	case corememory.ContextRawMessage, corememory.ContextFact, corememory.ContextSummary:
		return selector.conversation != "" && address.ConversationID == selector.conversation
	case corememory.ContextDocumentResource, corememory.ContextDocumentSection,
		corememory.ContextDocumentChunk, corememory.ContextDocumentSummary:
		if !selector.narrowDocuments {
			return false
		}
		_, selected := selector.datasets[address.DatasetID]
		return selected
	default:
		// profile is the standing block a request always carries, not a
		// candidate; anything unknown has no axis to judge it by.
		return false
	}
}

// Datasets returns the selected datasets in sorted order, for diagnostics and
// for the shard plan. It is empty when the document axis is not narrowed,
// which is not the same as narrowed to nothing — see NarrowedDocuments.
func (selector Selector) Datasets() []string {
	datasets := make([]string, 0, len(selector.datasets))
	for datasetID := range selector.datasets {
		datasets = append(datasets, datasetID)
	}
	sort.Strings(datasets)
	return datasets
}

// NarrowedDocuments reports whether the request named the document axis.
func (selector Selector) NarrowedDocuments() bool { return selector.narrowDocuments }

// MatchesRequest applies the read-side conversation/dataset selectors encoded
// by the ContextProvider without weakening the hard Scope partition. It is
// ParseSelector followed by Matches, for call sites that filter one address
// and have no use for the compiled selector.
func MatchesRequest(metadata corememory.Metadata, address component.CandidateAddress) (bool, error) {
	selector, err := ParseSelector(metadata)
	if err != nil {
		return false, err
	}
	return selector.Matches(address), nil
}

// SelectorKey returns a stable key for the metadata a read-side selection
// depends on. Callers that cache a selector-dependent value across requests
// key it on this fingerprint together with the projection identity. It exists
// next to MatchesRequest because the two must agree: they name the same
// metadata keys, and a cached selection is only valid for the fingerprint it
// was built under.
func SelectorKey(metadata corememory.Metadata) string {
	return chainDigest("selector", metadata["conversation_id"], metadata["dataset_ids"])
}
