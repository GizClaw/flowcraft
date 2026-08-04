package graph

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
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

	// Cancelled marks a branch deliberately cancelled through the
	// wave's ParallelController (while queued or running): it merged
	// as a no-op rather than failed, and Err stays nil.
	Cancelled bool
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

	// MaxBranches caps the total number of branches one wave may fan
	// out to; zero or negative means unlimited. A wave exceeding the
	// cap fails the run with an [errdefs.IsBudgetExceeded]-classified
	// error — unlike the legacy runner's silent truncation, which
	// could drop branches and their entire downstream subgraph.
	MaxBranches int

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
	if c.MaxBranches < 0 {
		return errdefs.Validationf("graph: parallel max branches must be >= 0")
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
	mergeChannelMessages(board, preFork, results,
		func(res BranchResult) bool { return res.Err == nil },
		func(inference.Message) bool { return true },
	)
}

func mergeChannelMessages(
	board *agent.Board,
	preFork *agent.BoardSnapshot,
	results []BranchResult,
	includeResult func(BranchResult) bool,
	includeMessage func(inference.Message) bool,
) {
	for _, res := range results {
		if !includeResult(res) || res.Snapshot == nil {
			continue
		}
		for ch, msgs := range res.Snapshot.Channels {
			base := len(preFork.Channels[ch])
			if len(msgs) <= base {
				continue
			}
			for _, msg := range msgs[base:] {
				if includeMessage(msg) {
					board.AppendChannelMessage(ch, msg)
				}
			}
		}
	}
}

// mergeInterruptedAssistantMessages retains only assistant messages written
// by failed branches when a parallel wave terminates as interrupted. Branch
// order is results order; vars and non-assistant channels/state are ignored.
func mergeInterruptedAssistantMessages(board *agent.Board, preFork *agent.BoardSnapshot, results []BranchResult) {
	mergeChannelMessages(board, preFork, results,
		func(res BranchResult) bool { return errdefs.IsInterrupted(res.Err) },
		func(msg inference.Message) bool { return msg.Role == inference.RoleAssistant },
	)
}

type parallelCtxKey struct{}

type parallelBranchIdentityKey struct{}

// parallelBranchIdentity is kernel-owned producer identity for stream
// deltas emitted while a parallel branch is executing. It is deliberately
// private so plugins cannot mint identities for another fork or branch.
type parallelBranchIdentity struct {
	forkID   string
	branchID string
}

func withParallelBranchIdentity(ctx context.Context, identity parallelBranchIdentity) context.Context {
	return context.WithValue(ctx, parallelBranchIdentityKey{}, identity)
}

func parallelBranchIdentityFromContext(ctx context.Context) (parallelBranchIdentity, bool) {
	if ctx == nil {
		return parallelBranchIdentity{}, false
	}
	identity, ok := ctx.Value(parallelBranchIdentityKey{}).(parallelBranchIdentity)
	return identity, ok
}

// ParallelController exposes limited control over the currently executing
// parallel fork. It is intentionally context-scoped: outside a fork it is absent.
type ParallelController interface {
	CancelNode(nodeID, reason string) bool
}

// WithParallelController stores the current fork controller on ctx.
func WithParallelController(ctx context.Context, c ParallelController) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, parallelCtxKey{}, c)
}

// ParallelControllerFromContext returns the current fork controller, if any.
func ParallelControllerFromContext(ctx context.Context) (ParallelController, bool) {
	if ctx == nil {
		return nil, false
	}
	c, ok := ctx.Value(parallelCtxKey{}).(ParallelController)
	return c, ok
}

// forkController implements [ParallelController] for one in-flight
// wave. Cancelling a branch marks it: a running branch's context is
// cancelled with the reason as its cause, a queued branch observes
// the mark before invoking and skips. Explicitly cancelled branches
// are not wave failures — their results merge as no-ops, exactly
// like skipped nodes.
//
// Branches see the controller through a per-branch view
// (branchView): a branch that is itself cancelled or finished may
// not cancel others — a dying branch's last gasp must not widen the
// damage.
type forkController struct {
	members map[string]struct{}

	mu      sync.Mutex
	running map[string]context.CancelCauseFunc // started, unfinished branches
	marks   map[string]string                  // nodeID -> cancel reason
	done    map[string]struct{}                // finished branches
}

func newForkController(members []string) *forkController {
	c := &forkController{
		members: make(map[string]struct{}, len(members)),
		running: make(map[string]context.CancelCauseFunc, len(members)),
		marks:   make(map[string]string),
		done:    make(map[string]struct{}, len(members)),
	}
	for _, id := range members {
		c.members[id] = struct{}{}
	}
	return c
}

// view returns the [ParallelController] one branch sees.
func (c *forkController) view(nodeID string) ParallelController {
	return branchView{parent: c, id: nodeID}
}

// cancelFrom marks the target branch cancelled and cancels its
// context with the reason as cause. Unknown or finished targets and
// disqualified callers (cancelled or finished themselves) report
// false; repeated cancellations of a live branch are idempotent and
// report true (the first reason wins).
func (c *forkController) cancelFrom(callerID, nodeID, reason string) bool {
	if reason == "" {
		reason = "cancelled by parallel.cancelNode"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.members[nodeID]; !ok {
		return false
	}
	if _, ok := c.done[nodeID]; ok {
		return false
	}
	if _, ok := c.done[callerID]; ok {
		return false
	}
	if _, ok := c.marks[callerID]; ok {
		return false
	}
	if _, marked := c.marks[nodeID]; !marked {
		c.marks[nodeID] = reason
	}
	if cancel, ok := c.running[nodeID]; ok {
		cancel(errors.New(c.marks[nodeID]))
	}
	return true
}

// start registers a running branch's cancel func. A branch cancelled
// while queued learns its reason here and must skip invocation.
func (c *forkController) start(nodeID string, cancel context.CancelCauseFunc) (reason string, cancelled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if reason, ok := c.marks[nodeID]; ok {
		return reason, true
	}
	c.running[nodeID] = cancel
	return "", false
}

// finish deregisters a completed branch; later cancellations report
// false.
func (c *forkController) finish(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.running, nodeID)
	c.done[nodeID] = struct{}{}
}

// cancelledReason reports whether the branch was explicitly cancelled,
// distinguishing deliberate cancellation from a genuine branch error.
func (c *forkController) cancelledReason(nodeID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reason, ok := c.marks[nodeID]
	return reason, ok
}

// branchView is the per-branch [ParallelController]: cancellation
// rights are checked against the caller branch's own state.
type branchView struct {
	parent *forkController
	id     string
}

func (v branchView) CancelNode(nodeID, reason string) bool {
	return v.parent.cancelFrom(v.id, nodeID, reason)
}
