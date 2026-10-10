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

// BuildManifest assembles the readable manifest of one derivation generation
// from the records that generation owns. It is a pure function of those
// records, so the manifest a compaction publishes and the manifest a later pass
// rebuilds for the same generation are the same manifest: records are ordered
// by level and coverage, and the coverage a manifest reports is the leaves'
// window.
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
	ordered := make([]Record, 0, len(records))
	for _, record := range records {
		if record.Scope != scope || record.ConversationID != conversationID {
			return Manifest{}, fmt.Errorf("summary view: record %q is addressed to another conversation", record.ID)
		}
		ordered = append(ordered, record)
	}
	sort.SliceStable(ordered, func(left, right int) bool {
		first, second := ordered[left], ordered[right]
		if first.Level != second.Level {
			return first.Level < second.Level
		}
		if first.CoverageRange.StartSeq != second.CoverageRange.StartSeq {
			return first.CoverageRange.StartSeq < second.CoverageRange.StartSeq
		}
		return first.ID < second.ID
	})
	manifest := Manifest{
		Scope: scope, ConversationID: conversationID, GenerationID: generation,
		RecordIDs: make([]string, 0, len(ordered)), CoverageRange: leafCoverage(ordered),
	}
	for _, record := range ordered {
		manifest.RecordIDs = append(manifest.RecordIDs, record.ID)
	}
	manifest.FrontierDigest = digestFrontier(manifest.RecordIDs, manifest.CoverageRange)
	// The manifest is not validated here: publishing stamps the publication
	// time it is validated with, and the store validates it on the way in.
	return manifest, nil
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
