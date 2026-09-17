package tool

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// sizedDef builds a definition whose definitionBytes is exactly size.
func sizedDef(name string, size int) message.ToolDefinition {
	const schema = `{"type":"object"}`
	pad := size - len(name) - len(schema) - 32
	if pad < 0 {
		panic("sizedDef: size too small for " + name)
	}
	return message.ToolDefinition{
		Name:        name,
		Description: strings.Repeat("x", pad),
		InputSchema: json.RawMessage(schema),
	}
}

func keptNames(t *testing.T, cands []candidate, st stateSnapshot, policy Policy) []string {
	t.Helper()
	out := make([]string, 0, len(cands))
	for _, c := range visibleCandidates(cands, st, policy) {
		out = append(out, c.name)
	}
	return out
}

func deferredPolicy(maxBytes int) Policy {
	return Policy{
		Default: ExposureDeferred,
		Budget:  Budget{MaxDefinitions: 8, MaxBytes: int64(maxBytes)},
	}
}

// TestVisibleCandidates_SkipsOversizedEntry covers the byte walk: an
// entry that does not fit must not stop every smaller candidate behind
// it from being kept.
func TestVisibleCandidates_SkipsOversizedEntry(t *testing.T) {
	policy := deferredPolicy(1000)
	st := stateSnapshot{discovered: map[string]discoveredEntry{
		"first":  {lastUse: 3, seq: 1, rank: 0},
		"huge":   {lastUse: 3, seq: 2, rank: 1},
		"second": {lastUse: 3, seq: 3, rank: 2},
	}}
	cands := []candidate{
		{name: "first", def: sizedDef("first", 100), exp: ExposureDeferred},
		{name: "huge", def: sizedDef("huge", 5000), exp: ExposureDeferred},
		{name: "second", def: sizedDef("second", 100), exp: ExposureDeferred},
	}

	got := keptNames(t, cands, st, policy)
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("kept = %v, want [first second] with the oversized entry skipped", got)
	}
}

// TestVisibleCandidates_RankedBatchBeatsMRUOrder covers the same-round
// tie-break: a tool_search batch is ranked, so its best hit wins the cut
// even when a later (higher-seq) hit would win on recency alone.
func TestVisibleCandidates_RankedBatchBeatsMRUOrder(t *testing.T) {
	policy := deferredPolicy(700)
	st := stateSnapshot{discovered: map[string]discoveredEntry{
		"best":  {lastUse: 3, seq: 8, rank: 0},
		"worse": {lastUse: 3, seq: 9, rank: 1},
	}}
	cands := []candidate{
		{name: "best", def: sizedDef("best", 600), exp: ExposureDeferred},
		{name: "worse", def: sizedDef("worse", 600), exp: ExposureDeferred},
	}

	got := keptNames(t, cands, st, policy)
	if len(got) != 1 || got[0] != "best" {
		t.Fatalf("kept = %v, want [best]: rank must beat seq within one round", got)
	}
}

// TestVisibleCandidates_SeparateDiscoveriesPreferMRU keeps the
// documented behavior for entries discovered by separate calls: same
// round, same rank, so the most recently discovered entry survives.
func TestVisibleCandidates_SeparateDiscoveriesPreferMRU(t *testing.T) {
	policy := deferredPolicy(700)
	st := stateSnapshot{discovered: map[string]discoveredEntry{
		"a": {lastUse: 3, seq: 1, rank: 0},
		"b": {lastUse: 3, seq: 2, rank: 0},
	}}
	cands := []candidate{
		{name: "a", def: sizedDef("a", 600), exp: ExposureDeferred},
		{name: "b", def: sizedDef("b", 600), exp: ExposureDeferred},
	}

	got := keptNames(t, cands, st, policy)
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("kept = %v, want [b]: same-rank entries fall back to MRU", got)
	}
}

// TestVisibleCandidates_KeepsOversizedFirstEntry pins the escape hatch:
// the highest-priority entry is kept even when it alone exceeds the
// budget, so a single large schema cannot dead-end the visible set.
func TestVisibleCandidates_KeepsOversizedFirstEntry(t *testing.T) {
	policy := deferredPolicy(1000)
	st := stateSnapshot{discovered: map[string]discoveredEntry{
		"huge": {lastUse: 3, seq: 1, rank: 0},
	}}
	cands := []candidate{
		{name: "huge", def: sizedDef("huge", 5000), exp: ExposureDeferred},
	}

	got := keptNames(t, cands, st, policy)
	if len(got) != 1 || got[0] != "huge" {
		t.Fatalf("kept = %v, want [huge]", got)
	}
}
