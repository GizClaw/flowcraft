package summary

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// ExtendRequest names the inputs that continue the window a published manifest
// already covers.
type ExtendRequest struct {
	Scope          corememory.Scope
	ConversationID string
	GenerationID   string
	// PolicySignature is the signature every record of the published tree was
	// derived with, and it is required: a record id covers the signature, so an
	// extension that names a different one derives records the tree does not
	// hold and publishes a tree no complete compaction would have produced. The
	// caller that built the tree passes the signature it compacted with; a
	// caller that cannot state it compacts the complete window instead.
	PolicySignature string
	// Inputs continue the published window: every one of them belongs after
	// every input the manifest covers, which is what a pass walking commits
	// forward produces. An input the manifest already covers is ignored, so a
	// re-derivation of an old window extends nothing; an input that sorts below
	// the window makes the request non-extendable, and the caller compacts the
	// complete window instead.
	Inputs []Input
}

// maxTreeLevels is how many levels a tree can hold: a leaf level and the three
// levels above it, the same shape the summary view names in a manifest.
const maxTreeLevels = int(summaryview.L3) + 1

// inputKey is one input's place in the compaction order: the first message
// sequence it covers, then its identity.
type inputKey struct {
	seq uint64
	id  string
}

func (key inputKey) less(other inputKey) bool {
	if key.seq != other.seq {
		return key.seq < other.seq
	}
	return key.id < other.id
}

// Extend appends inputs to the published tree of one generation, deriving only
// the groups the new inputs touch.
//
// A published tree is a partition of the inputs that built it: the leaves are
// the inputs, and every level above groups the level below in runs of the
// compactor's chunk size. An input that continues the window lands in the last
// group of every level, so the groups before that one are exactly the groups
// the manifest already names -- and records are content-addressed, so naming
// them again is the whole of the work. Extend re-derives the groups the new
// inputs fill, reading the manifest, the tail group of every level and the
// published children that group shares with the rebuilt region: the window
// below the change is neither read nor re-hashed.
//
// It reports extended=false when the published manifest is not one it can
// extend -- no manifest for the generation, one published before the tree was
// recorded, a group the tree names that is gone, or an input that belongs below
// the published window (a merged or reordered input) -- and the caller then
// compacts the complete window, which is always correct.
func (compactor *Compactor) Extend(
	ctx context.Context,
	request ExtendRequest,
) (summaryview.Manifest, bool, error) {
	if compactor == nil || compactor.store == nil || compactor.summarizer == nil {
		return summaryview.Manifest{}, false, errors.New("summary compactor: compactor is incomplete")
	}
	if ctx == nil {
		return summaryview.Manifest{}, false, errors.New("summary compactor: context is required")
	}
	if err := ctx.Err(); err != nil {
		return summaryview.Manifest{}, false, err
	}
	if err := request.Scope.Validate(); err != nil {
		return summaryview.Manifest{}, false, err
	}
	request.ConversationID = strings.TrimSpace(request.ConversationID)
	request.GenerationID = strings.TrimSpace(request.GenerationID)
	request.PolicySignature = strings.TrimSpace(request.PolicySignature)
	if request.ConversationID == "" || request.GenerationID == "" || request.PolicySignature == "" {
		return summaryview.Manifest{}, false, errors.New(
			"summary compactor: conversation_id, generation_id, and policy_signature are required")
	}
	manifest, found, err := compactor.store.LoadActive(ctx, request.Scope, request.ConversationID)
	if err != nil {
		return summaryview.Manifest{}, false, err
	}
	if !found || manifest.GenerationID != request.GenerationID {
		return summaryview.Manifest{}, false, nil
	}
	published, ok := manifest.Tree()
	if !ok {
		return summaryview.Manifest{}, false, nil
	}
	compact := CompactRequest{
		Scope: request.Scope, ConversationID: request.ConversationID,
		GenerationID: request.GenerationID, PolicySignature: request.PolicySignature,
	}
	// The inputs the manifest does not cover yet are what extends the window.
	// The ones it does cover are a pass re-deriving what is already there: they
	// are named by the leaves it published, so extending them again would name
	// one input twice.
	known := make(map[string]struct{}, len(published[0]))
	for _, id := range published[0] {
		known[id] = struct{}{}
	}
	freshInputs := make([]Input, 0, len(request.Inputs))
	for _, input := range request.Inputs {
		id, err := compactor.leafID(compact, input)
		if err != nil {
			return summaryview.Manifest{}, false, err
		}
		if _, covered := known[id]; covered {
			continue
		}
		freshInputs = append(freshInputs, input)
	}
	if len(freshInputs) == 0 {
		// Nothing the manifest does not already cover: the published tree is
		// the tree of these inputs, and a pass that derives nothing rewrites
		// nothing.
		return manifest, true, nil
	}
	sort.Slice(freshInputs, func(left, right int) bool {
		first, second := freshInputs[left], freshInputs[right]
		if first.CoverageRange.StartSeq != second.CoverageRange.StartSeq {
			return first.CoverageRange.StartSeq < second.CoverageRange.StartSeq
		}
		return first.ID < second.ID
	})
	for index := 1; index < len(freshInputs); index++ {
		if freshInputs[index].ID == freshInputs[index-1].ID {
			return summaryview.Manifest{}, false, nil
		}
	}
	// The window the manifest covers ends at its last leaf, and the new inputs
	// have to continue it: a group is a run of inputs, so an input that belongs
	// below the window would move every group after it.
	windowEnd, ok, err := compactor.windowEnd(ctx, request, published[0])
	if err != nil {
		return summaryview.Manifest{}, false, err
	}
	if !ok {
		return summaryview.Manifest{}, false, nil
	}
	for _, input := range freshInputs {
		key := inputKey{seq: input.CoverageRange.StartSeq, id: input.ID}
		if !windowEnd.less(key) {
			return summaryview.Manifest{}, false, nil
		}
	}
	// The new leaves: every one of them is one input, and the input's digest
	// addresses the record that summarises it.
	derived := make(map[string]summaryview.Record, len(freshInputs))
	graph := make([][]string, 0, len(published)+2)
	leaves := append([]string(nil), published[0]...)
	active := make(map[string]summaryview.Record)
	for _, input := range freshInputs {
		record, err := compactor.extendLeaf(ctx, compact, active, input)
		if err != nil {
			return summaryview.Manifest{}, false, err
		}
		leaves = append(leaves, record.ID)
		derived[record.ID] = record
		active[record.ID] = record
	}
	graph = append(graph, leaves)
	coverage := extendCoverage(manifest.CoverageRange, freshInputs)
	children, changed := leaves, len(published[0])
	for level := 1; level < maxTreeLevels; level++ {
		if !compactor.levelExists(summaryview.Level(level), len(children)) {
			break
		}
		var below []string
		if level-1 < len(published) {
			below = published[level-1]
		} else {
			// The level below is new, so everything above it is: this is the
			// pass that grows the tree a level.
			changed = 0
		}
		var extendedAt []string
		if level < len(published) {
			extendedAt = published[level]
		}
		records, firstChanged, ok, err := compactor.rebuildLevel(ctx, request, compact,
			summaryview.Level(level), compactor.levelSize(summaryview.Level(level)),
			extendedAt, children, below, derived, changed)
		if err != nil {
			return summaryview.Manifest{}, false, err
		}
		if !ok {
			return summaryview.Manifest{}, false, nil
		}
		graph = append(graph, records)
		children, changed = records, firstChanged
	}
	extended, err := summaryview.BuildManifestFromTree(
		request.Scope, request.ConversationID, request.GenerationID, graph, coverage)
	if err != nil {
		return summaryview.Manifest{}, false, err
	}
	if err := compactor.store.PublishActive(ctx, extended); err != nil {
		return summaryview.Manifest{}, false, err
	}
	// The published manifest is the store's: publishing stamps the publication
	// time, and a caller comparing two passes compares what readers resolve.
	landed, found, err := compactor.store.LoadActive(ctx, request.Scope, request.ConversationID)
	if err != nil {
		return summaryview.Manifest{}, false, err
	}
	if !found {
		return summaryview.Manifest{}, false, nil
	}
	return landed, true, nil
}

// leafID is the record id that summarises one input. The compactor builds a
// leaf from the canonical compaction input, so the digest covers the input and
// nothing else.
func (compactor *Compactor) leafID(request CompactRequest, input Input) (string, error) {
	if strings.TrimSpace(input.ID) == "" {
		return "", errors.New("summary compactor: every input needs an id")
	}
	return summaryview.StableID(request.Scope, request.ConversationID, summaryview.L0,
		[]string{input.ID}, digestValue("l0", input), compactor.transformSignature(request, summaryview.L0)), nil
}

// extendLeaf makes the leaf record of one input durable, reusing the record
// when an earlier pass already wrote it.
func (compactor *Compactor) extendLeaf(
	ctx context.Context,
	request CompactRequest,
	active map[string]summaryview.Record,
	input Input,
) (summaryview.Record, error) {
	sourceDigest := digestValue("l0", input)
	transform := compactor.transformSignature(request, summaryview.L0)
	id := summaryview.StableID(request.Scope, request.ConversationID, summaryview.L0,
		[]string{input.ID}, sourceDigest, transform)
	return compactor.ensureRecord(ctx, active, summaryview.AddRequest{
		ID:    id,
		Scope: request.Scope, ConversationID: request.ConversationID, Level: summaryview.L0,
		Text: strings.TrimSpace(input.Text), Content: textContent(strings.TrimSpace(input.Text)),
		Topics: input.Topics, InputIDs: []string{input.ID}, SourceRefs: input.SourceRefs,
		CoverageRange: input.CoverageRange, SourceDigest: sourceDigest,
		TransformSignature: transform, GenerationID: request.GenerationID,
	})
}

// windowEnd is the place the published window ends: the order key of the last
// leaf, which is the input that leaf summarises.
func (compactor *Compactor) windowEnd(
	ctx context.Context,
	request ExtendRequest,
	leaves []string,
) (inputKey, bool, error) {
	last, found, err := compactor.store.Get(ctx, request.Scope, request.ConversationID, leaves[len(leaves)-1])
	if err != nil {
		return inputKey{}, false, err
	}
	if !found || len(last.InputIDs) != 1 {
		return inputKey{}, false, nil
	}
	return inputKey{seq: last.CoverageRange.StartSeq, id: last.InputIDs[0]}, true, nil
}

// levelExists mirrors the complete compaction: a leaf level always has the
// level above it (while the depth allows one), and every level above is built
// once the level below it is long enough to condense.
func (compactor *Compactor) levelExists(level summaryview.Level, below int) bool {
	config := compactor.config
	switch level {
	case summaryview.L0:
		return below > 0
	case summaryview.L1:
		return below > 0 && config.MaxDepth > 1
	default:
		if below < config.CondenseThreshold || int(level) >= config.MaxDepth {
			return false
		}
		return level <= summaryview.L3
	}
}

// levelSize is the chunk size one level groups its children in.
func (compactor *Compactor) levelSize(level summaryview.Level) int {
	config := compactor.config
	if level == summaryview.L1 {
		return config.ChunkSize
	}
	return config.GroupSize
}

// rebuildLevel re-derives the groups of one level that the extension touches.
//
// The groups entirely below the changed children are the groups the manifest
// already names, and they are kept by identity. Every group from the chunk the
// change starts in is derived again -- from the same children, chunked the same
// way a complete compaction would chunk them -- because a group is addressed by
// its children, so a group whose run of children changed is a different record.
//
// It returns the level's record ids in order along with the position where the
// level starts to differ from the published one, which is what the level above
// needs to find the groups of its own that changed. extended=false reports a
// tree the caller has to compact afresh: a group it names is gone, or a group
// does not summarise a run of the level below it.
func (compactor *Compactor) rebuildLevel(
	ctx context.Context,
	request ExtendRequest,
	compact CompactRequest,
	level summaryview.Level,
	size int,
	published, children, publishedChildren []string,
	derived map[string]summaryview.Record,
	changed int,
) ([]string, int, bool, error) {
	if size <= 0 {
		return nil, 0, false, fmt.Errorf("summary compactor: level %s has no chunk size", level)
	}
	boundary := (changed / size) * size
	if len(published) == 0 {
		// The level is new: it is derived over every child it has, which is
		// what a complete compaction does for a level it is growing into.
		boundary = 0
	}
	position := make(map[string]int, len(publishedChildren))
	for index, id := range publishedChildren {
		position[id] = index
	}
	// The published groups that cover the rebuilt region are the ones at the
	// end of the level: a group sits where its first child sits, so the first
	// group of the region is the last one read.
	kept := len(published)
	covered := 0
	for kept > 0 {
		record, found, err := compactor.store.Get(ctx, request.Scope, request.ConversationID, published[kept-1])
		if err != nil {
			return nil, 0, false, err
		}
		if !found {
			// A group the tree names is gone: the tree is not one to extend.
			return nil, 0, false, nil
		}
		if len(record.InputIDs) == 0 {
			return nil, 0, false, nil
		}
		at, ok := position[record.InputIDs[0]]
		if !ok || at+len(record.InputIDs) > len(publishedChildren) {
			return nil, 0, false, nil
		}
		if at < boundary {
			break
		}
		covered += len(record.InputIDs)
		kept--
	}
	expected := len(publishedChildren) - boundary
	if len(published) == 0 || expected < 0 {
		// A new level covers nothing yet, and a change that starts past the
		// published children leaves every group of the level below it: both
		// tile an empty region of the published level.
		expected = 0
	}
	if covered != expected {
		// The groups the tree names do not tile the region they cover.
		return nil, 0, false, nil
	}
	records := make([]string, 0, kept+len(children)/size+1)
	records = append(records, published[:kept]...)
	region := children[boundary:]
	active := make(map[string]summaryview.Record, size)
	for start := 0; start < len(region); start += size {
		end := min(start+size, len(region))
		inputs := make([]summaryview.Record, 0, end-start)
		for _, id := range region[start:end] {
			record, ok := derived[id]
			if !ok {
				var found bool
				var err error
				record, found, err = compactor.store.Get(ctx, request.Scope, request.ConversationID, id)
				if err != nil {
					return nil, 0, false, err
				}
				if !found {
					return nil, 0, false, nil
				}
				derived[id] = record
			}
			inputs = append(inputs, record)
			active[id] = record
		}
		summaries, err := compactor.summarize(ctx, compact, active, level, inputs)
		if err != nil {
			return nil, 0, false, err
		}
		for _, record := range summaries {
			records = append(records, record.ID)
			derived[record.ID] = record
		}
	}
	// The level starts to differ where the kept groups end: every group from
	// there on was derived again.
	return records, kept, true, nil
}

// extendCoverage widens a manifest's window with the leaves of one extension.
func extendCoverage(coverage summaryview.CoverageRange, inputs []Input) summaryview.CoverageRange {
	for _, input := range inputs {
		current := input.CoverageRange
		if coverage.StartTime.IsZero() || (!current.StartTime.IsZero() && current.StartTime.Before(coverage.StartTime)) {
			coverage.StartTime = current.StartTime
		}
		if current.EndTime.After(coverage.EndTime) {
			coverage.EndTime = current.EndTime
		}
		if coverage.StartSeq == 0 || current.StartSeq < coverage.StartSeq {
			coverage.StartSeq = current.StartSeq
		}
		if current.EndSeq > coverage.EndSeq {
			coverage.EndSeq = current.EndSeq
		}
	}
	return coverage
}
