package graph

import (
	"context"
	"reflect"
	"slices"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// MergeStrategy selects the built-in [MergeFunc] used to fold parallel
// branch results back into the shared board.
type MergeStrategy string

const (
	// FirstWriteWins lets the earliest branch (wave order) that changed
	// a var win; later conflicting writes are dropped.
	FirstWriteWins MergeStrategy = "first_write_wins"

	// LastWriteWins lets the latest branch (wave order) that changed a
	// var win.
	LastWriteWins MergeStrategy = "last_write_wins"
)

// BranchResult is one parallel branch's outcome: the isolated board
// snapshot it produced, or the error it failed with. A nil Snapshot
// with a nil Err marks a skipped node — nothing to merge.
type BranchResult struct {
	NodeID   string
	Snapshot *agent.BoardSnapshot
	Err      error
}

// MergeFunc folds completed branch results back into the shared board.
//
// preFork is the board state captured before the wave fanned out;
// implementations should apply each branch's *changes relative to
// preFork*, not absolute state, so unrelated pre-existing vars and
// channel history survive.
type MergeFunc func(ctx context.Context, board *agent.Board, preFork *agent.BoardSnapshot, results []BranchResult) error

// ParallelConfig controls concurrent execution of independent frontier
// nodes (fan-out waves of size >= 2).
//
// Isolation model: every branch runs against a private copy of the
// pre-fork board (Board.Snapshot / Board.RestoreFrom), so branches
// never race and merge outcomes are deterministic. The deliberate
// trade-off: a branch's board writes become visible only at merge
// time — live incremental output (LLM tokens, tool progress) reaches
// observers in real time through stream-delta events instead
// (ExecutionContext.EmitStreamDelta).
type ParallelConfig struct {
	// Enabled turns parallel wave execution on. Disabled graphs run
	// every wave sequentially in definition order.
	Enabled bool

	// BranchTimeout bounds each branch's wall-clock time. Zero means
	// no per-branch timeout.
	BranchTimeout time.Duration

	// MaxConcurrency caps how many branches run at once. Zero or
	// negative means "all branches of the wave".
	MaxConcurrency int

	// MergeStrategy selects the built-in merge. Empty means
	// [FirstWriteWins].
	MergeStrategy MergeStrategy

	// Merge overrides MergeStrategy with a custom function.
	Merge MergeFunc
}

func (c ParallelConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if c.BranchTimeout < 0 {
		return errdefs.Validationf("graph: parallel branch timeout must be >= 0")
	}
	if c.MaxConcurrency < 0 {
		return errdefs.Validationf("graph: parallel max concurrency must be >= 0")
	}
	if c.Merge == nil {
		switch c.MergeStrategy {
		case "", FirstWriteWins, LastWriteWins:
		default:
			return errdefs.Validationf("graph: unknown merge strategy %q", c.MergeStrategy)
		}
	}
	return nil
}

// mergeFunc resolves the effective merge implementation.
func (c ParallelConfig) mergeFunc() MergeFunc {
	if c.Merge != nil {
		return c.Merge
	}
	if c.MergeStrategy == LastWriteWins {
		return lastWriteWinsMerge
	}
	return firstWriteWinsMerge
}

// firstWriteWinsMerge applies var changes in wave order: the first
// branch to change a key wins. Channels are treated as append-only —
// messages added by each branch are appended in wave order.
func firstWriteWinsMerge(_ context.Context, board *agent.Board, preFork *agent.BoardSnapshot, results []BranchResult) error {
	claimed := map[string]bool{}
	for _, res := range results {
		mergeBranchVars(board, preFork, res, claimed)
	}
	mergeAppendedMessages(board, preFork, results)
	return nil
}

// lastWriteWinsMerge is firstWriteWinsMerge with reversed priority:
// the last branch to change a key wins.
func lastWriteWinsMerge(_ context.Context, board *agent.Board, preFork *agent.BoardSnapshot, results []BranchResult) error {
	claimed := map[string]bool{}
	for _, result := range slices.Backward(results) {
		mergeBranchVars(board, preFork, result, claimed)
	}
	mergeAppendedMessages(board, preFork, results)
	return nil
}

func mergeBranchVars(board *agent.Board, preFork *agent.BoardSnapshot, res BranchResult, claimed map[string]bool) {
	if res.Err != nil || res.Snapshot == nil {
		return
	}
	for key, val := range res.Snapshot.Vars {
		if claimed[key] {
			continue
		}
		if prev, ok := preFork.Vars[key]; ok && reflect.DeepEqual(prev, val) {
			continue // unchanged by this branch
		}
		board.SetVar(key, val)
		claimed[key] = true
	}
}

// mergeAppendedMessages replays each branch's channel additions onto
// the shared board, in wave order. Channels are append-only by
// convention; a branch that replaced a channel outright contributes
// only the suffix beyond the pre-fork length.
func mergeAppendedMessages(board *agent.Board, preFork *agent.BoardSnapshot, results []BranchResult) {
	for _, res := range results {
		if res.Err != nil || res.Snapshot == nil {
			continue
		}
		for ch, msgs := range res.Snapshot.Channels {
			base := len(preFork.Channels[ch])
			if len(msgs) <= base {
				continue
			}
			for _, m := range msgs[base:] {
				board.AppendChannelMessage(ch, m)
			}
		}
	}
}
