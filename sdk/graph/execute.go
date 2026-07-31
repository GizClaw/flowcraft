package graph

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
)

// Execute implements [agent.Engine]: it runs the graph against board
// and returns the (possibly newly allocated) board with the final
// state.
//
// The run is a wave loop over a node frontier:
//
//  1. The frontier starts at the entry node — or, when resuming, at
//     the successors of the checkpointed node, with board state
//     restored from the checkpoint.
//  2. Each wave dedups the frontier, then invokes its nodes —
//     sequentially, or concurrently under [ParallelConfig]. Skipped
//     nodes (skip condition true) route without invoking; they emit a
//     step-complete event with Skipped=true.
//  3. After a wave, the engine stamps a checkpoint on the host and
//     evaluates outgoing edge conditions to compute the next frontier.
//  4. The loop ends when the frontier is empty (all branches reached
//     END or produced no outgoing edge); exceeding MaxIterations fails
//     the run with an [errdefs.IsBudgetExceeded]-classified error.
//
// Cooperative interrupts are polled between waves; an interrupt
// records [VarInterruptedNode] on the board and returns an
// [errdefs.IsInterrupted]-classified error.
//
// Observability: one "graph.execute" span per run, one
// "node.<type>.execute" span per invocation, execution counters and
// duration histograms on the "graph" meter, and structured logs at
// start/failure. Best-effort event publish failures are counted
// separately (see telemetry.go).
func (g *Graph) Execute(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board) (retBoard *agent.Board, runErr error) {
	if board == nil {
		board = agent.NewBoard()
	}
	retBoard = board

	ctx, span := telemetry.Tracer().Start(ctx, "graph.execute",
		trace.WithAttributes(
			attribute.String(telemetry.AttrGraphName, g.name),
			attribute.String(telemetry.AttrRunID, run.RunID),
			attribute.String(telemetry.AttrAgentID, run.AgentID),
		))
	graphStart := time.Now()
	defer func() {
		status := execStatus(runErr)
		if runErr != nil && !isInterruptedErr(runErr) {
			span.RecordError(runErr)
			span.SetStatus(codes.Error, runErr.Error())
		} else {
			span.SetStatus(codes.Ok, status)
		}
		span.End()
		recordGraphExec(ctx, g, run, status, time.Since(graphStart))
	}()

	if g.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.timeout)
		defer cancel()
	}

	telemetry.Info(ctx, "graph execution started",
		otellog.String(telemetry.AttrGraphName, g.name),
		otellog.String(telemetry.AttrRunID, run.RunID))

	// originalStartedAt persists "when did the user-visible run begin"
	// across resume boundaries; checkpoints thread it through.
	originalStartedAt := graphStart
	if rc, ok := agent.ResumeContextFromContext(ctx); ok && !rc.StartedAt.IsZero() {
		originalStartedAt = rc.StartedAt
	}

	frontier := []string{g.entry}
	iterations := 0
	if cp := run.ResumeFrom; cp != nil {
		if cp.ExecID != "" && cp.ExecID != run.RunID {
			return retBoard, errdefs.Validationf(
				"graph %q: checkpoint exec id %q does not match run id %q (forking requires a fresh run)",
				g.name, cp.ExecID, run.RunID)
		}
		if err := g.CanResume(*cp); err != nil {
			return retBoard, err
		}
		if cp.Board != nil {
			board.RestoreFrom(cp.Board)
		}
		iterations = cp.Iteration
		next, err := g.resolveNext(board, []string{cp.Step})
		if err != nil {
			return retBoard, err
		}
		frontier = next
	}

	for len(frontier) > 0 {
		if ctx.Err() != nil {
			return retBoard, classifyContextError(ctx, g.name, "")
		}
		if intr, ok := pollInterrupt(host); ok {
			board.SetVar(VarInterruptedNode, frontier[0])
			return retBoard, agent.Interrupted(intr)
		}
		wave := dedupIDs(frontier)
		if g.maxIterations > 0 && iterations+len(wave) > g.maxIterations {
			return retBoard, errdefs.BudgetExceededf(
				"graph %q exceeded max iterations (%d) — possible cycle",
				g.name, g.maxIterations)
		}
		if err := g.executeWave(ctx, run, host, board, wave); err != nil {
			telemetry.Error(ctx, "graph execution failed",
				otellog.String(telemetry.AttrGraphName, g.name),
				otellog.String(telemetry.AttrRunID, run.RunID),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
			return retBoard, err
		}
		iterations += len(wave)
		g.stampCheckpoint(ctx, host, run, board, wave[len(wave)-1], iterations, originalStartedAt)
		next, err := g.resolveNext(board, wave)
		if err != nil {
			return retBoard, err
		}
		frontier = next
	}
	return retBoard, nil
}

// executeWave invokes one frontier, sequentially or in parallel.
func (g *Graph) executeWave(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board, wave []string) error {
	if len(wave) == 1 || !g.parallel.Enabled {
		for _, id := range wave {
			if _, err := g.invokeNode(ctx, run, host, board, g.nodes[id]); err != nil {
				return err
			}
		}
		return nil
	}
	return g.executeParallel(ctx, run, host, board, wave)
}

// invokeNode runs a single node: skip check → config reference
// resolution → typed decode → read-role validation → handler (with
// retries) → write-role validation, bracketed by step lifecycle
// events, a "node.<type>.execute" span, and execution metrics. It
// returns skipped=true when the skip condition fired — the node was
// not invoked but still routes.
func (g *Graph) invokeNode(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board, slot *nodeSlot) (skipped bool, err error) {
	nodeID := slot.def.ID
	info := run.Info()
	// Run identity is ambient from here down: handlers, script
	// bindings, and stream adapters pull it from the context.
	ctx = agent.WithRunInfo(ctx, info)

	if slot.skipCondition != nil {
		skip, err := slot.skipCondition.Evaluate(board)
		if err != nil {
			return false, fmt.Errorf("graph %q node %q: %w", g.name, nodeID, err)
		}
		if skip {
			publishStepSkipped(ctx, host, g, info, nodeID)
			return true, nil
		}
	}

	ctx, span := telemetry.Tracer().Start(ctx, "node."+slot.def.Type+".execute",
		trace.WithAttributes(
			attribute.String(telemetry.AttrGraphName, g.name),
			attribute.String(telemetry.AttrNodeID, nodeID),
			attribute.String("node.type", slot.def.Type),
			attribute.String(telemetry.AttrRunID, run.RunID),
		))
	nodeStart := time.Now()
	defer func() {
		status := execStatus(err)
		if err != nil && !isInterruptedErr(err) {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, status)
		}
		span.End()
		recordNodeExec(ctx, g, slot, run, status, time.Since(nodeStart))
	}()

	resolved, rerr := resolveConfig(slot.def.Config, board)
	if rerr != nil {
		return false, fmt.Errorf("graph %q node %q: %w", g.name, nodeID, rerr)
	}

	if verr := g.validateReads(board, slot); verr != nil {
		return false, verr
	}

	ec := ExecutionContext{Context: ctx, Host: host, NodeID: nodeID, GraphID: g.name}
	publishStepStarted(ctx, host, g, info, nodeID)
	preInvoke := channelLengths(board, slot.writes)

	invoke := func() error {
		if slot.fallback != nil {
			return slot.fallback(ec, board, slot.def.Type, resolved)
		}
		cfg, derr := slot.decode(resolved)
		if derr != nil {
			return derr
		}
		return slot.invoke(ec, board, cfg)
	}

	var invokeErr error
	for attempt := 0; ; attempt++ {
		invokeErr = invoke()
		if invokeErr == nil || !isRetryable(invokeErr) || attempt >= g.maxNodeRetries {
			break
		}
	}
	if invokeErr != nil {
		if cerr := classifyContextError(ctx, g.name, nodeID); cerr != nil {
			invokeErr = cerr
		}
		telemetry.Error(ctx, "node execution failed",
			otellog.String(telemetry.AttrGraphName, g.name),
			otellog.String(telemetry.AttrNodeID, nodeID),
			otellog.String(telemetry.AttrErrorMessage, invokeErr.Error()))
		publishStepError(ctx, host, g, info, nodeID, invokeErr)
		return false, fmt.Errorf("graph %q node %q: %w", g.name, nodeID, invokeErr)
	}

	if verr := g.validateWrites(board, slot, preInvoke); verr != nil {
		publishStepError(ctx, host, g, info, nodeID, verr)
		return false, verr
	}
	publishStepCompleted(ctx, host, g, info, nodeID)
	return false, nil
}

// validateReads enforces required read roles before the handler runs.
func (g *Graph) validateReads(board *agent.Board, slot *nodeSlot) error {
	for _, r := range slot.reads {
		switch r.Kind {
		case RoleVar:
			if _, ok := board.GetVar(r.Key); !ok && r.Required {
				return errdefs.Validationf(
					"graph %q node %q: required variable %q missing on board",
					g.name, slot.def.ID, r.Key)
			}
		case RoleMessages:
			if r.Required && len(board.Channel(r.Key)) == 0 {
				return errdefs.Validationf(
					"graph %q node %q: required channel %q is empty",
					g.name, slot.def.ID, r.Key)
			}
		}
	}
	return nil
}

// validateWrites enforces required write roles after the handler
// returns.
func (g *Graph) validateWrites(board *agent.Board, slot *nodeSlot, preInvoke map[string]int) error {
	for _, w := range slot.writes {
		switch w.Kind {
		case RoleVar:
			if _, ok := board.GetVar(w.Key); !ok && w.Required {
				return errdefs.Validationf(
					"graph %q node %q: handler did not write required variable %q",
					g.name, slot.def.ID, w.Key)
			}
		case RoleMessages:
			if w.Required && len(board.Channel(w.Key)) <= preInvoke[w.Key] {
				return errdefs.Validationf(
					"graph %q node %q: handler did not append to required channel %q",
					g.name, slot.def.ID, w.Key)
			}
		}
	}
	return nil
}

// executeParallel fans a wave out across goroutines. Every branch runs
// against a private copy of the pre-fork board; results merge back
// deterministically via the configured [MergeFunc]. A wave with any
// failing branch fails without merging.
func (g *Graph) executeParallel(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board, wave []string) error {
	preFork := board.Snapshot()
	info := run.Info()
	forkID := fmt.Sprintf("%s#%s", run.RunID, wave[0])

	results := make([]BranchResult, len(wave))
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallelLimit(g.parallel.MaxConcurrency, len(wave)))

	for i, id := range wave {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			bctx := ctx
			if g.parallel.BranchTimeout > 0 {
				var cancel context.CancelFunc
				bctx, cancel = context.WithTimeout(ctx, g.parallel.BranchTimeout)
				defer cancel()
			}

			slot := g.nodes[id]
			publishBranchDelta(bctx, host, info, g.name, id, agent.StreamDeltaPayload{
				Type:     agent.StreamDeltaParallelBranchAccept,
				ForkID:   forkID,
				BranchID: id,
			})

			branchBoard := agent.NewBoard()
			branchBoard.RestoreFrom(preFork)
			skipped, err := g.invokeNode(bctx, run, host, branchBoard, slot)
			switch {
			case skipped:
				results[i] = BranchResult{NodeID: id}
			case err != nil:
				results[i] = BranchResult{NodeID: id, Err: err}
				publishBranchDelta(bctx, host, info, g.name, id, agent.StreamDeltaPayload{
					Type:     agent.StreamDeltaParallelBranchCancel,
					ForkID:   forkID,
					BranchID: id,
					Reason:   err.Error(),
				})
			default:
				results[i] = BranchResult{NodeID: id, Snapshot: branchBoard.Snapshot()}
			}
		}()
	}
	wg.Wait()

	for _, res := range results {
		if res.Err != nil {
			return res.Err
		}
	}
	return g.parallel.mergeFunc()(ctx, board, preFork, results)
}

// stampCheckpoint persists the wave boundary on the host. Checkpoint
// failures never fail the run — durability is the host's choice.
func (g *Graph) stampCheckpoint(ctx context.Context, host agent.Host, run agent.Run, board *agent.Board, lastNodeID string, iterations int, startedAt time.Time) {
	if host == nil {
		return
	}
	_ = host.Checkpoint(ctx, agent.Checkpoint{
		ExecID:            run.RunID,
		Step:              lastNodeID,
		Iteration:         iterations,
		Board:             board.Snapshot(),
		Attributes:        run.Attributes,
		Timestamp:         time.Now(),
		OriginalStartedAt: startedAt,
		SpecVersion:       g.name,
	})
}

// resolveNext computes the next frontier after a wave: every outgoing
// edge whose condition passes (or is absent) contributes its target;
// END targets terminate quietly.
func (g *Graph) resolveNext(board *agent.Board, executed []string) ([]string, error) {
	var next []string
	for _, id := range executed {
		for _, e := range g.edges[id] {
			take := true
			if e.Condition != nil {
				ok, err := e.Condition.Evaluate(board)
				if err != nil {
					return nil, fmt.Errorf("graph %q node %q: %w", g.name, id, err)
				}
				take = ok
			}
			if take && e.To != END {
				next = append(next, e.To)
			}
		}
	}
	return dedupIDs(next), nil
}

// classifyContextError converts context termination into a classified
// error: deadline → Timeout, cancellation → Aborted. nodeID may be
// empty for run-level termination. Returns nil when the context is
// still alive.
func classifyContextError(ctx context.Context, graphName, nodeID string) error {
	where := fmt.Sprintf("graph %q", graphName)
	if nodeID != "" {
		where = fmt.Sprintf("graph %q node %q", graphName, nodeID)
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return errdefs.Timeoutf("%s execution timed out", where)
	case errors.Is(ctx.Err(), context.Canceled):
		return errdefs.Abortedf("%s execution aborted", where)
	default:
		return nil
	}
}

// pollInterrupt non-blockingly checks the host's cooperative interrupt
// channel. A nil host or nil channel means "never fires".
func pollInterrupt(host agent.Host) (agent.Interrupt, bool) {
	if host == nil {
		return agent.Interrupt{}, false
	}
	select {
	case intr, ok := <-host.Interrupts():
		return intr, ok
	default:
		return agent.Interrupt{}, false
	}
}

// isRetryable reports whether a node failure is worth retrying.
// Interrupts, aborts, budget exhaustion and validation errors are
// deterministic or terminal — retrying them cannot help.
func isRetryable(err error) bool {
	return !errdefs.IsInterrupted(err) &&
		!errdefs.IsAborted(err) &&
		!errdefs.IsBudgetExceeded(err) &&
		!errdefs.IsValidation(err)
}

func parallelLimit(maxConcurrency, waveSize int) int {
	if maxConcurrency > 0 && maxConcurrency < waveSize {
		return maxConcurrency
	}
	return waveSize
}

func dedupIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func channelLengths(board *agent.Board, roles []resolvedRole) map[string]int {
	lengths := map[string]int{}
	for _, w := range roles {
		if w.Kind == RoleMessages {
			lengths[w.Key] = len(board.Channel(w.Key))
		}
	}
	return lengths
}
