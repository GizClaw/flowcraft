package main

import (
	"strconv"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// kHitResult reports whether any recalled item matched the gold evidence,
// split by raw-message hits and derived-view hits (facts/summaries whose
// provenance references an evidence message).
type kHitResult struct {
	Hit     bool
	Message bool
	Fact    bool
}

func evidenceSeqs(evidenceIDs []string, byEvidence map[string]uint64) map[uint64]bool {
	result := make(map[uint64]bool, len(evidenceIDs))
	for _, id := range evidenceIDs {
		if seq, ok := byEvidence[id]; ok {
			result[seq] = true
		}
	}
	return result
}

func computeKHit(items []sdkmemory.ContextItem, evidence map[uint64]bool) kHitResult {
	var result kHitResult
	for _, item := range items {
		if item.Kind == sdkmemory.ContextRawMessage {
			if evidence[item.Sequence] {
				result.Hit = true
				result.Message = true
			}
			continue
		}
		for _, source := range item.Sources {
			if source.Kind != sdkmemory.SourceMessage || source.Revision == "" {
				continue
			}
			seq, err := strconv.ParseUint(source.Revision, 10, 64)
			if err == nil && evidence[seq] {
				result.Hit = true
				result.Fact = true
				break
			}
		}
	}
	return result
}
