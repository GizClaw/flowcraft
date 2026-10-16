package summary

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// frontierDigestPrefix domain-separates the frontier digest from the record
// identities and the catalog digests, which are hashes of the same kind.
const frontierDigestPrefix = "flowcraft.memory.summary.frontier\x00v1\x00"

// treeLevels is how many levels a manifest can hold: L0 to L3.
const treeLevels = int(L3) + 1

// BuildManifest assembles the readable manifest of one derivation generation
// from the records that generation owns. It is a pure function of those
// records, so the manifest a compaction publishes and the manifest a later pass
// rebuilds for the same generation are the same manifest -- whatever order the
// records arrive in.
//
// The manifest names the tree in the order it was built: the leaves in the
// compactor's input order, then every level above them in the order its groups
// were formed, which is the order its first child appears in the level below.
// The coverage a manifest reports is the leaves' window.
func BuildManifest(
	scope corememory.Scope,
	conversationID, generation string,
	records []Record,
) (Manifest, error) {
	conversationID = strings.TrimSpace(conversationID)
	generation = strings.TrimSpace(generation)
	if err := scope.Validate(); err != nil {
		return Manifest{}, err
	}
	if conversationID == "" || generation == "" {
		return Manifest{}, errors.New("summary view: conversation_id and generation_id are required")
	}
	if len(records) == 0 {
		return Manifest{}, errors.New("summary view: a manifest needs at least one record")
	}
	tree, err := buildTree(records, scope, conversationID)
	if err != nil {
		return Manifest{}, err
	}
	levels := make([][]string, len(tree))
	for level, records := range tree {
		levels[level] = make([]string, 0, len(records))
		for _, record := range records {
			levels[level] = append(levels[level], record.ID)
		}
	}
	return BuildManifestFromTree(scope, conversationID, generation, levels, leafCoverage(tree[0]))
}

// BuildManifestFromTree assembles the manifest of one generation from the tree
// a compaction built: the record ids of every level in grouping order, and the
// window the leaves cover. A caller that has just extended a published tree
// knows both without reading the records the extension reused, so it can
// publish the extended manifest from the tree alone.
func BuildManifestFromTree(
	scope corememory.Scope,
	conversationID, generation string,
	tree [][]string,
	coverage CoverageRange,
) (Manifest, error) {
	conversationID = strings.TrimSpace(conversationID)
	generation = strings.TrimSpace(generation)
	if err := scope.Validate(); err != nil {
		return Manifest{}, err
	}
	if conversationID == "" || generation == "" {
		return Manifest{}, errors.New("summary view: conversation_id and generation_id are required")
	}
	if len(tree) == 0 || len(tree[0]) == 0 {
		return Manifest{}, errors.New("summary view: a manifest needs at least one leaf")
	}
	if len(tree) > treeLevels {
		return Manifest{}, fmt.Errorf("summary view: a manifest holds at most %d levels", treeLevels)
	}
	manifest := Manifest{
		Scope: scope, ConversationID: conversationID, GenerationID: generation,
		CoverageRange: coverage, LevelCounts: make([]int, treeLevels),
	}
	for level, ids := range tree {
		if len(ids) == 0 {
			return Manifest{}, fmt.Errorf("summary view: level %d of the tree is empty", level)
		}
		manifest.LevelCounts[level] = len(ids)
		for _, id := range ids {
			if strings.TrimSpace(id) == "" {
				return Manifest{}, fmt.Errorf("summary view: level %d of the tree names an empty record", level)
			}
			manifest.RecordIDs = append(manifest.RecordIDs, id)
		}
	}
	// The levels above the top one stay at zero: a tree of n levels is named by
	// n levels and three-to-n zeros, and nothing below a named level is ever
	// missing.
	manifest.FrontierDigest = digestFrontier(manifest.RecordIDs, manifest.CoverageRange)
	// The manifest is not validated here: publishing stamps the publication
	// time it is validated with, and the store validates it on the way in.
	return manifest, nil
}

// buildTree orders a generation's records into the tree that names them: level
// by level, each level in the order its groups were formed.
//
// The leaves are ordered by the compaction input each of them summarises -- the
// message window it covers, then the input's identity -- which is the order the
// compactor took its inputs in. Every level above is ordered by the position of
// its first child in the level below, because a group's children are the
// consecutive run of the level below that it summarises: where a group sits in
// its own level is therefore where its first child sits.
func buildTree(records []Record, scope corememory.Scope, conversationID string) ([][]Record, error) {
	byLevel := make([][]Record, treeLevels)
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		if record.Scope != scope || record.ConversationID != conversationID {
			return nil, fmt.Errorf("summary view: record %q is addressed to another conversation", record.ID)
		}
		if strings.TrimSpace(record.ID) == "" {
			return nil, errors.New("summary view: every record of a tree needs an id")
		}
		if err := record.Level.Validate(); err != nil {
			return nil, err
		}
		if _, duplicate := seen[record.ID]; duplicate {
			return nil, fmt.Errorf("summary view: record %q is named twice", record.ID)
		}
		seen[record.ID] = struct{}{}
		if len(record.InputIDs) == 0 {
			return nil, fmt.Errorf("summary view: record %q names no inputs", record.ID)
		}
		byLevel[record.Level] = append(byLevel[record.Level], record)
	}
	if len(byLevel[L0]) == 0 {
		return nil, errors.New("summary view: a manifest needs at least one leaf")
	}
	tree := make([][]Record, 0, treeLevels)
	for level := 0; level < treeLevels; level++ {
		if len(byLevel[level]) == 0 {
			// Every level is built from the level below it, so a level with
			// records above an empty one is not a tree.
			for above := level + 1; above < treeLevels; above++ {
				if len(byLevel[above]) > 0 {
					return nil, fmt.Errorf("summary view: level %d has records but level %d does not", above, level)
				}
			}
			break
		}
		if Level(level) == L0 {
			sortLeaves(byLevel[level])
		} else if err := orderGroups(byLevel[level], byLevel[level-1]); err != nil {
			return nil, err
		}
		tree = append(tree, byLevel[level])
	}
	return tree, nil
}

// sortLeaves orders the leaves the way the compactor took its inputs: by the
// first message sequence they cover, then by the input they summarise.
func sortLeaves(records []Record) {
	sort.SliceStable(records, func(left, right int) bool {
		first, second := records[left], records[right]
		if first.CoverageRange.StartSeq != second.CoverageRange.StartSeq {
			return first.CoverageRange.StartSeq < second.CoverageRange.StartSeq
		}
		return first.InputIDs[0] < second.InputIDs[0]
	})
}

// orderGroups places one level above another: a group sits where its first
// child sits. A group that does not summarise a consecutive run of the level
// below it is not a tree, and is refused instead of being placed somewhere
// arbitrary.
func orderGroups(records []Record, children []Record) error {
	position := make(map[string]int, len(children))
	for index, child := range children {
		position[child.ID] = index
	}
	claimed := make(map[int]string, len(children))
	placed := make(map[string]int, len(records))
	for _, record := range records {
		first, ok := position[record.InputIDs[0]]
		if !ok {
			return fmt.Errorf("summary view: record %q summarises %q, which is not in the level below it",
				record.ID, record.InputIDs[0])
		}
		for offset, inputID := range record.InputIDs {
			at, ok := position[inputID]
			if !ok {
				return fmt.Errorf("summary view: record %q summarises %q, which is not in the level below it",
					record.ID, inputID)
			}
			if at != first+offset {
				return fmt.Errorf("summary view: record %q summarises %q, which is outside the run it starts",
					record.ID, inputID)
			}
			if owner, taken := claimed[at]; taken {
				return fmt.Errorf("summary view: records %q and %q both summarise %q", owner, record.ID, inputID)
			}
			claimed[at] = record.ID
		}
		placed[record.ID] = first
	}
	sort.SliceStable(records, func(left, right int) bool {
		return placed[records[left].ID] < placed[records[right].ID]
	})
	return nil
}

// leafCoverage is the window a manifest covers: the union of its leaves'
// coverage, which is what the compactor's inputs covered. A record above the
// leaves covers a subset of the leaves it summarises, so including it would not
// widen the window.
func leafCoverage(records []Record) CoverageRange {
	var coverage CoverageRange
	first := true
	for _, record := range records {
		if record.Level != L0 {
			continue
		}
		current := record.CoverageRange
		if first || current.StartSeq < coverage.StartSeq {
			coverage.StartSeq = current.StartSeq
		}
		if current.EndSeq > coverage.EndSeq {
			coverage.EndSeq = current.EndSeq
		}
		if coverage.StartTime.IsZero() || (!current.StartTime.IsZero() && current.StartTime.Before(coverage.StartTime)) {
			coverage.StartTime = current.StartTime
		}
		if current.EndTime.After(coverage.EndTime) {
			coverage.EndTime = current.EndTime
		}
		first = false
	}
	return coverage
}

func digestFrontier(recordIDs []string, coverage CoverageRange) string {
	payload, _ := json.Marshal([]any{recordIDs, coverage})
	sum := sha256.Sum256(append([]byte(frontierDigestPrefix), payload...))
	return hex.EncodeToString(sum[:])
}
