package projection

import (
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

var (
	chatKinds = []corememory.ContextItemKind{
		corememory.ContextRawMessage,
		corememory.ContextFact,
		corememory.ContextSummary,
	}
	documentKinds = []corememory.ContextItemKind{
		corememory.ContextDocumentResource,
		corememory.ContextDocumentSection,
		corememory.ContextDocumentChunk,
		corememory.ContextDocumentSummary,
	}
	allKinds = append(append([]corememory.ContextItemKind{corememory.ContextProfile}, chatKinds...), documentKinds...)
)

// TestSelectorJudgesEachKindByItsOwnAxis is the judgement table the strict
// selector stands on: naming an axis narrows it to that axis' kinds, and the
// cell a caller is most likely to get wrong -- a conversation-scoped request
// that named no datasets -- reads no documents at all.
func TestSelectorJudgesEachKindByItsOwnAxis(t *testing.T) {
	shapes := []struct {
		name     string
		metadata corememory.Metadata
		global   bool
		chat     bool
		document bool
	}{
		{name: "global", metadata: nil, global: true, chat: true, document: true},
		{name: "conversation only", metadata: corememory.Metadata{"conversation_id": "c1"}, chat: true},
		{name: "datasets only", metadata: corememory.Metadata{"dataset_ids": `["d1"]`}, document: true},
		{
			name:     "both",
			metadata: corememory.Metadata{"conversation_id": "c1", "dataset_ids": `["d1"]`},
			chat:     true, document: true,
		},
		{name: "empty dataset list", metadata: corememory.Metadata{"dataset_ids": `[]`}},
	}
	addresses := []struct {
		name    string
		address component.CandidateAddress
		axis    string
	}{
		{name: "chat in scope", axis: "chat", address: component.CandidateAddress{Kind: corememory.ContextFact, ConversationID: "c1"}},
		{name: "chat out of scope", axis: "none", address: component.CandidateAddress{Kind: corememory.ContextFact, ConversationID: "c2"}},
		{name: "document in scope", axis: "document", address: component.CandidateAddress{Kind: corememory.ContextDocumentChunk, DatasetID: "d1", DocumentID: "doc"}},
		{name: "document out of scope", axis: "none", address: component.CandidateAddress{Kind: corememory.ContextDocumentChunk, DatasetID: "d2", DocumentID: "doc"}},
		{name: "profile", axis: "none", address: component.CandidateAddress{Kind: corememory.ContextProfile, ConversationID: "c1"}},
	}
	for _, shape := range shapes {
		for _, candidate := range addresses {
			want := false
			switch candidate.axis {
			case "chat":
				want = shape.chat
			case "document":
				want = shape.document
			}
			if shape.global {
				// A request that narrows nothing reads every address,
				// including the ones no axis would select.
				want = true
			}
			t.Run(shape.name+"/"+candidate.name, func(t *testing.T) {
				got, err := MatchesRequest(shape.metadata, candidate.address)
				if err != nil {
					t.Fatalf("MatchesRequest: %v", err)
				}
				if got != want {
					t.Fatalf("matches = %v, want %v", got, want)
				}
			})
		}
	}
	// Every chat kind and every document kind has to follow the group it was
	// put in, not the two kinds the table exercised.
	selector, err := ParseSelector(corememory.Metadata{"conversation_id": "c1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range chatKinds {
		if !selector.Matches(component.CandidateAddress{Kind: kind, ConversationID: "c1"}) {
			t.Errorf("%s should be judged by the conversation axis", kind)
		}
	}
	for _, kind := range documentKinds {
		if selector.Matches(component.CandidateAddress{Kind: kind, DatasetID: "d1", DocumentID: "doc"}) {
			t.Errorf("%s should not survive a conversation-only selector", kind)
		}
	}
}

// TestParseSelectorRejectsAMalformedDatasetList keeps a decode failure from
// degrading into the widest possible read.
func TestParseSelectorRejectsAMalformedDatasetList(t *testing.T) {
	for _, raw := range []string{`["d1"`, `d1`, `{"d1":true}`, `["d1",]`} {
		if _, err := ParseSelector(corememory.Metadata{"dataset_ids": raw}); err == nil {
			t.Errorf("dataset_ids %q decoded without an error", raw)
		}
		if _, err := MatchesRequest(corememory.Metadata{"dataset_ids": raw}, component.CandidateAddress{}); err == nil {
			t.Errorf("dataset_ids %q matched without an error", raw)
		}
		if _, err := ShardsFor(corememory.Metadata{"dataset_ids": raw}); err == nil {
			t.Errorf("dataset_ids %q planned shards without an error", raw)
		}
	}
	// An explicit empty list is a selection of nothing, not a global read.
	selector, err := ParseSelector(corememory.Metadata{"dataset_ids": `[]`})
	if err != nil {
		t.Fatal(err)
	}
	if selector.Global() || !selector.NarrowedDocuments() {
		t.Fatalf("empty list: global=%v narrowed=%v", selector.Global(), selector.NarrowedDocuments())
	}
	if selector.Matches(component.CandidateAddress{
		Kind: corememory.ContextDocumentChunk, DatasetID: "d1", DocumentID: "doc",
	}) {
		t.Error("an empty dataset list selected a document")
	}
}

// TestShardPlanRoutesExactlyWhatTheSelectorMatches is the equivalence the
// routing rests on, over random tables and every selector shape: for each
// address, the plan holds its shard exactly when the selector matches it. That
// is both halves at once -- no matching record lives outside the plan (no
// false negative) and no shard in the plan is scanned for nothing (no false
// positive), so a scan of the plan filtered by the selector equals a scan of
// the whole scope filtered by it, record for record.
func TestShardPlanRoutesExactlyWhatTheSelectorMatches(t *testing.T) {
	source := rand.New(rand.NewSource(20261027))
	conversations := []string{"c1", "c2", "c3", "c4"}
	datasets := []string{"d1", "d2", "d3"}
	selectors := []corememory.Metadata{
		nil,
		{},
		{"conversation_id": "c1"},
		{"conversation_id": "c3"},
		{"dataset_ids": `["d1"]`},
		{"dataset_ids": `["d2","d3"]`},
		{"dataset_ids": `[]`},
		{"conversation_id": "c2", "dataset_ids": `["d1"]`},
		{"conversation_id": "c9", "dataset_ids": `["d9"]`},
	}
	for round := 0; round < 200; round++ {
		table := randomTable(source, conversations, datasets)
		metadata := selectors[round%len(selectors)]
		selector, err := ParseSelector(metadata)
		if err != nil {
			t.Fatalf("ParseSelector(%v): %v", metadata, err)
		}
		plan, err := ShardsFor(metadata)
		if err != nil {
			t.Fatalf("ShardsFor(%v): %v", metadata, err)
		}
		again, err := ShardsFor(metadata)
		if err != nil || !reflect.DeepEqual(plan, again) {
			t.Fatalf("ShardsFor(%v) is not deterministic: %+v vs %+v (%v)", metadata, plan, again, err)
		}
		if !sort.SliceIsSorted(plan.Shards, func(i, j int) bool { return plan.Shards[i] < plan.Shards[j] }) {
			// The conversation shard sorts under "c:" and the dataset shards
			// under "d:", so the whole list is sorted by construction.
			t.Fatalf("ShardsFor(%v) returned %v, which is not sorted", metadata, plan.Shards)
		}
		for index, shard := range plan.Shards {
			if index > 0 && shard == plan.Shards[index-1] {
				t.Fatalf("ShardsFor(%v) repeated %q", metadata, shard)
			}
		}
		if plan.All != selector.Global() {
			t.Fatalf("ShardsFor(%v).All = %v, global = %v", metadata, plan.All, selector.Global())
		}
		for _, address := range table {
			shard, err := ShardForAddress(address)
			if err != nil {
				t.Fatalf("ShardForAddress(%+v): %v", address, err)
			}
			if matched, planned := selector.Matches(address), plan.Holds(shard); matched != planned {
				t.Fatalf("selector %v, address %+v: matches=%v but plan %+v holds %q",
					metadata, address, matched, plan, shard)
			}
		}
	}
}

// TestShardForAddressAgreesWithValidate keeps the two switches that group
// kinds from drifting apart: a kind the address validation accepts has exactly
// one shard, one the validation rejects (profile) has none, and the shard
// carries the id the kind is addressed by.
func TestShardForAddressAgreesWithValidate(t *testing.T) {
	for _, kind := range allKinds {
		address := component.CandidateAddress{
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
	for _, address := range []component.CandidateAddress{
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

// TestShardSegmentRoundTripsEveryID pins the encoding boundary: ids that would
// otherwise become path structure survive a round trip, and segments that are
// not shards are rejected by the scan that parses them.
func TestShardSegmentRoundTripsEveryID(t *testing.T) {
	for _, shard := range []string{
		"c:conversation",
		"c:conversation/with/slash",
		"c:nul\x00inside",
		"c:会话-1",
		"c:c:twice",
		"d:dataset.with.dots",
	} {
		segment, err := ShardSegment(shard)
		if err != nil {
			t.Fatalf("ShardSegment(%q): %v", shard, err)
		}
		if strings.ContainsAny(segment, "/\x00") {
			t.Fatalf("ShardSegment(%q) = %q, which is not a single path segment", shard, segment)
		}
		decoded, err := ParseShardSegment(segment)
		if err != nil {
			t.Fatalf("ParseShardSegment(%q): %v", segment, err)
		}
		if decoded != shard {
			t.Fatalf("round trip of %q = %q", shard, decoded)
		}
	}
	for _, segment := range []string{"", "x_abc", "k_abc", "c_", "d_", "c_!!!", "c" + "conversation"} {
		if _, err := ParseShardSegment(segment); err == nil {
			t.Errorf("ParseShardSegment(%q) accepted a segment that is not a shard", segment)
		}
	}
	for _, shard := range []string{"", "c:", "d:", "x:id", "conversation"} {
		if _, err := ShardSegment(shard); err == nil {
			t.Errorf("ShardSegment(%q) encoded something that is not a shard", shard)
		}
	}
}

func randomTable(source *rand.Rand, conversations, datasets []string) []component.CandidateAddress {
	table := make([]component.CandidateAddress, 0, 48)
	for index := 0; index < 48; index++ {
		kind := chatKinds[source.Intn(len(chatKinds))]
		if source.Intn(3) == 0 {
			kind = documentKinds[source.Intn(len(documentKinds))]
		}
		address := component.CandidateAddress{
			Kind:           kind,
			ConversationID: conversations[source.Intn(len(conversations))],
			DatasetID:      datasets[source.Intn(len(datasets))],
			DocumentID:     "doc",
			ItemID:         "item",
		}
		table = append(table, address)
	}
	return table
}
