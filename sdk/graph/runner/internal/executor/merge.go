package executor

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// MergeFunc merges parallel branch results into the parent board.
// snapshot is the pre-fork board state (for conflict detection).
type MergeFunc func(board *graph.Board, snapshot *graph.BoardSnapshot, results []branchResult) error

var mergeRegistry = map[MergeStrategy]MergeFunc{
	MergeLastWins:        mergeLastWins,
	MergeNamespace:       mergeNamespace,
	MergeErrorOnConflict: mergeErrorOnConflict,
}

// RegisterMergeStrategy registers a custom merge strategy.
func RegisterMergeStrategy(name MergeStrategy, fn MergeFunc) {
	mergeRegistry[name] = fn
}

func lookupMerge(strategy MergeStrategy) MergeFunc {
	if fn, ok := mergeRegistry[strategy]; ok {
		return fn
	}
	return mergeLastWins
}

func copyMsgs(msgs []model.Message) []model.Message {
	cp := make([]model.Message, len(msgs))
	copy(cp, msgs)
	return cp
}

func mergeLastWins(board *graph.Board, _ *graph.BoardSnapshot, results []branchResult) error {
	for _, r := range results {
		for k, v := range r.vars {
			board.SetVar(k, v)
		}
		for k, msgs := range r.channels {
			board.SetChannel(k, copyMsgs(msgs))
		}
	}
	return nil
}

func mergeNamespace(board *graph.Board, _ *graph.BoardSnapshot, results []branchResult) error {
	for i, r := range results {
		prefix := fmt.Sprintf("__branch_%d.", i)
		for k, v := range r.vars {
			board.SetVar(prefix+k, v)
		}
		for k, msgs := range r.channels {
			board.SetChannel(prefix+k, copyMsgs(msgs))
		}
	}
	return nil
}

func mergeErrorOnConflict(board *graph.Board, snapshot *graph.BoardSnapshot, results []branchResult) error {
	modified := make(map[string]int)
	origVars := snapshot.Vars
	for i, r := range results {
		for k, v := range r.vars {
			origVal, exists := origVars[k]
			if !exists || fmt.Sprintf("%v", origVal) != fmt.Sprintf("%v", v) {
				if _, seen := modified[k]; seen && modified[k] != i {
					return errdefs.Conflictf("parallel merge conflict on variable %q", k)
				}
				modified[k] = i
			}
		}
	}

	origCh := snapshot.Channels
	if origCh == nil {
		origCh = map[string][]model.Message{}
	}
	modCh := make(map[string]int)
	for i, r := range results {
		for k, msgs := range r.channels {
			origVal, exists := origCh[k]
			if !exists || !channelMessagesEqual(origVal, msgs) {
				if _, seen := modCh[k]; seen && modCh[k] != i {
					return errdefs.Conflictf("parallel merge conflict on message channel %q", k)
				}
				modCh[k] = i
			}
		}
	}

	for _, r := range results {
		for k, v := range r.vars {
			board.SetVar(k, v)
		}
		for k, msgs := range r.channels {
			board.SetChannel(k, copyMsgs(msgs))
		}
	}
	return nil
}

func channelMessagesEqual(a, b []model.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Role != b[i].Role || a[i].Content() != b[i].Content() {
			return false
		}
	}
	return true
}
