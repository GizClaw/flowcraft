package graph

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
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

	spanAttrs := []attribute.KeyValue{
		attribute.String(telemetry.AttrGraphName, g.name),
		attribute.String(telemetry.AttrRunID, run.RunID),
		attribute.String(telemetry.AttrAgentID, run.AgentID),
	}
	spanAttrs = append(spanAttrs, runScopeAttrs(run)...)
	ctx, span := telemetry.Tracer().Start(ctx, "graph.execute",
		trace.WithAttributes(spanAttrs...))
	graphStart := time.Now()
	defer func() {
		status := execStatus(runErr)
		span.SetAttributes(attribute.String(telemetry.AttrRunStatus, runStatusValue(status)))
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

	_ = publishRunEvent(ctx, host, g, run, agent.SubjectRunStart(run.RunID), nil)
	defer func() {
		publishCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			g.runEndPublishTimeout,
		)
		defer cancel()
		if err := publishRunEvent(
			publishCtx, host, g, run,
			agent.SubjectRunEnd(run.RunID), runErr,
		); err != nil {
			runErr = errors.Join(runErr, errdefs.Internal(
				&agent.RunEndPublishError{Err: err},
			))
		}
	}()

	startLogAttrs := []otellog.KeyValue{
		otellog.String(telemetry.AttrGraphName, g.name),
		otellog.String(telemetry.AttrRunID, run.RunID),
	}
	startLogAttrs = append(startLogAttrs, runScopeLogAttrs(run)...)
	telemetry.Info(ctx, "graph execution started", startLogAttrs...)

	// originalStartedAt persists "when did the user-visible run begin"
	// across resume boundaries; checkpoints thread it through.
	originalStartedAt := graphStart
	if rc, ok := agent.ResumeContextFromContext(ctx); ok && !rc.StartedAt.IsZero() {
		originalStartedAt = rc.StartedAt
	}

	frontier := []string{g.entry}
	iterations := 0
	if cp := run.ResumeFrom; cp != nil {
		if err := cp.Validate(); err != nil {
			return retBoard, err
		}
		if cp.ExecID != run.RunID {
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
		next, err := g.resolveNext(board, cp.Steps, iterations)
		if err != nil {
			return retBoard, err
		}
		frontier = next
	} else if startID, ok := startNodeFromContext(ctx); ok {
		if _, known := g.nodes[startID]; !known {
			return retBoard, errdefs.Validationf(
				"graph %q: start node %q is not defined", g.name, startID)
		}
		frontier = []string{startID}
	}

	for len(frontier) > 0 {
		if ctx.Err() != nil {
			if cause := context.Cause(ctx); errdefs.IsInterrupted(cause) {
				board.SetVar(VarInterruptedNode, frontier[0])
			}
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
		if err := g.executeWave(ctx, run, host, board, wave, iterations); err != nil {
			errorLogAttrs := []otellog.KeyValue{
				otellog.String(telemetry.AttrGraphName, g.name),
				otellog.String(telemetry.AttrRunID, run.RunID),
				otellog.String(telemetry.AttrErrorMessage, err.Error()),
			}
			errorLogAttrs = append(errorLogAttrs, runScopeLogAttrs(run)...)
			telemetry.Error(ctx, "graph execution failed", errorLogAttrs...)
			return retBoard, err
		}
		if ctx.Err() != nil {
			return retBoard, classifyContextError(ctx, g.name, "")
		}
		if intr, ok := pollInterrupt(host); ok {
			board.SetVar(VarInterruptedNode, wave[len(wave)-1])
			return retBoard, agent.Interrupted(intr)
		}
		iterations += len(wave)
		g.stampCheckpoint(ctx, host, run, board, wave, iterations, originalStartedAt)
		next, err := g.resolveNext(board, wave, iterations)
		if err != nil {
			return retBoard, err
		}
		frontier = next
	}
	return retBoard, nil
}

// executeWave invokes one frontier, sequentially or in parallel.
func (g *Graph) executeWave(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board, wave []string, iteration int) error {
	if len(wave) == 1 || !g.parallel.Enabled {
		for _, id := range wave {
			if _, err := g.invokeNode(ctx, run, host, board, g.nodes[id]); err != nil {
				return err
			}
		}
		return nil
	}
	return g.executeParallel(ctx, run, host, board, wave, iteration)
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
	// Run identity and Host are ambient from here down: handlers,
	// dispatchers, script bindings, and stream adapters pull them from
	// this per-invocation derived context. The caller's context is never
	// mutated.
	ctx = agent.WithRunInfo(ctx, info)
	ctx = agent.ContextWithHost(ctx, host)

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

	ec := ExecutionContext{Context: ctx, Host: host, NodeID: nodeID, NodeType: slot.def.Type, GraphID: g.name}
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
		if attempt > 0 {
			// Linear backoff between attempts; the wait stays
			// interruptible so a cancelled run never sleeps out its
			// remaining budget.
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
			if ctx.Err() != nil {
				invokeErr = ctx.Err()
				break
			}
		}
		snapshot := board.Snapshot()
		invokeErr = invoke()
		if invokeErr == nil || !isRetryable(invokeErr) || attempt >= g.maxNodeRetries {
			break
		}
		// Roll the board back to the pre-attempt state so a retried
		// node never sees its own half-written vars or duplicated
		// messages. The final attempt's writes stay for diagnostics.
		board.RestoreFrom(snapshot)
	}
	if invokeErr != nil {
		if cerr := classifyContextError(ctx, g.name, nodeID); cerr != nil {
			invokeErr = cerr
		}
		if errdefs.IsInterrupted(invokeErr) {
			board.SetVar(VarInterruptedNode, nodeID)
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
// failing branch fails without merging — but a branch cancelled
// through the wave's [ParallelController] is not a failure: it merges
// as a no-op, like a skipped node.
func (g *Graph) executeParallel(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board, wave []string, iteration int) error {
	if g.parallel.MaxBranches > 0 && len(wave) > g.parallel.MaxBranches {
		return errdefs.BudgetExceededf(
			"graph %q: parallel wave of %d branches exceeds max_branches %d",
			g.name, len(wave), g.parallel.MaxBranches)
	}
	preFork := board.Snapshot()
	info := run.Info()
	attempt := 1
	if parsed, err := strconv.Atoi(run.Attributes["agent.attempt"]); err == nil && parsed > 0 {
		attempt = parsed
	}
	forkID := fmt.Sprintf(
		"%s#attempt-%d#iteration-%d#%s",
		run.RunID,
		attempt,
		iteration,
		wave[0],
	)
	controller := newForkController(wave)

	publishParallelWave(ctx, host, g, info, agent.SubjectParallelFork(info.RunID),
		ParallelWaveEventPayload{ForkID: forkID, Branches: wave})

	results := make([]BranchResult, len(wave))
	failedBoards := make([]*agent.Board, len(wave))
	terminal := make([]bool, len(wave))
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallelLimit(g.parallel.MaxConcurrency, len(wave)))

	for i, id := range wave {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			bctx, cancelBranch := context.WithCancelCause(ctx)
			defer cancelBranch(nil)
			if g.parallel.BranchTimeout > 0 {
				var cancel context.CancelFunc
				bctx, cancel = context.WithTimeout(bctx, g.parallel.BranchTimeout)
				defer cancel()
			}

			slot := g.nodes[id]

			// A branch cancelled while queued skips invocation entirely.
			if reason, cancelled := controller.start(id, cancelBranch); cancelled {
				g.publishBranchTerminal(bctx, host, info, id, agent.StreamDeltaPayload{
					Type:     agent.StreamDeltaParallelBranchCancel,
					ForkID:   forkID,
					BranchID: id,
					Reason:   reason,
				})
				results[i] = BranchResult{NodeID: id, Cancelled: true}
				terminal[i] = true
				return
			}
			defer controller.finish(id)
			bctx = WithParallelController(bctx, controller.view(id))
			bctx = withParallelBranchIdentity(bctx, parallelBranchIdentity{
				forkID:   forkID,
				branchID: id,
			})

			branchBoard := agent.NewBoard()
			branchBoard.RestoreFrom(preFork)
			skipped, err := g.invokeNode(bctx, run, host, branchBoard, slot)
			if reason, cancelled := controller.cancelledReason(id); cancelled {
				// Cancellation wins over every handler outcome. A
				// handler may ignore ctx and return success/skipped;
				// its private board must still be discarded.
				results[i] = BranchResult{NodeID: id, Cancelled: true}
				g.publishBranchTerminal(bctx, host, info, id, agent.StreamDeltaPayload{
					Type:     agent.StreamDeltaParallelBranchCancel,
					ForkID:   forkID,
					BranchID: id,
					Reason:   reason,
				})
				terminal[i] = true
				return
			}
			switch {
			case skipped:
				results[i] = BranchResult{NodeID: id}
			case err != nil:
				results[i] = BranchResult{NodeID: id, Err: err}
				failedBoards[i] = branchBoard
				g.publishBranchTerminal(bctx, host, info, id, agent.StreamDeltaPayload{
					Type:     agent.StreamDeltaParallelBranchCancel,
					ForkID:   forkID,
					BranchID: id,
					Reason:   err.Error(),
				})
				terminal[i] = true
			default:
				results[i] = BranchResult{NodeID: id, Snapshot: branchBoard.Snapshot()}
			}
		}()
	}
	wg.Wait()

	var waveErr error
	if cause := context.Cause(ctx); errdefs.IsInterrupted(cause) {
		waveErr = cause
	}
	for _, res := range results {
		if waveErr == nil && errdefs.IsInterrupted(res.Err) {
			waveErr = res.Err
		}
	}
	if waveErr == nil {
		for _, res := range results {
			if res.Err != nil {
				waveErr = res.Err
				break
			}
		}
	}
	if waveErr != nil {
		publishUnterminatedBranchCancels(
			ctx, host, info, g.name, forkID, wave, terminal, waveErr.Error(), g.runEndPublishTimeout)
		if errdefs.IsInterrupted(waveErr) {
			for i, branchBoard := range failedBoards {
				if branchBoard != nil {
					results[i].Snapshot = branchBoard.Snapshot()
				}
			}
			mergeInterruptedAssistantMessages(board, preFork, results)
			interruptedNode := wave[0]
			for _, res := range results {
				if errdefs.IsInterrupted(res.Err) {
					interruptedNode = res.NodeID
					break
				}
			}
			board.SetVar(VarInterruptedNode, interruptedNode)
		}
		return waveErr
	}
	if err := g.parallel.mergeFunc()(ctx, board, preFork, results); err != nil {
		publishUnterminatedBranchCancels(
			ctx, host, info, g.name, forkID, wave, terminal, err.Error(), g.runEndPublishTimeout)
		return err
	}
	acceptCtx, cancelAccept := context.WithTimeout(
		context.WithoutCancel(ctx),
		g.runEndPublishTimeout,
	)
	defer cancelAccept()
	for i, id := range wave {
		if terminal[i] {
			continue
		}
		publishBranchDelta(acceptCtx, host, info, g.name, id, agent.StreamDeltaPayload{
			Type:     agent.StreamDeltaParallelBranchAccept,
			ForkID:   forkID,
			BranchID: id,
		})
		terminal[i] = true
	}
	publishParallelWave(ctx, host, g, info, agent.SubjectParallelJoin(info.RunID),
		ParallelWaveEventPayload{ForkID: forkID, Branches: wave, Cancelled: cancelledBranches(results)})
	return nil
}

// publishBranchTerminal gives one branch terminal its own bounded
// cleanup window even when the run or branch context is already done.
func (g *Graph) publishBranchTerminal(
	ctx context.Context,
	host agent.Host,
	info agent.RunInfo,
	nodeID string,
	delta agent.StreamDeltaPayload,
) {
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.runEndPublishTimeout)
	defer cancel()
	publishBranchDelta(publishCtx, host, info, g.name, nodeID, delta)
}

// publishUnterminatedBranchCancels closes every branch that completed
// successfully but cannot be accepted because its wave or merge failed.
// Iterating in wave order keeps the terminal stream deterministic. All
// missing terminals share one cleanup deadline so a blocked host cannot
// multiply the shutdown latency by the branch count.
func publishUnterminatedBranchCancels(
	ctx context.Context,
	host agent.Host,
	info agent.RunInfo,
	graphID, forkID string,
	wave []string,
	terminal []bool,
	reason string,
	timeout time.Duration,
) {
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	for i, id := range wave {
		if terminal[i] {
			continue
		}
		publishBranchDelta(publishCtx, host, info, graphID, id, agent.StreamDeltaPayload{
			Type:     agent.StreamDeltaParallelBranchCancel,
			ForkID:   forkID,
			BranchID: id,
			Reason:   reason,
		})
		terminal[i] = true
	}
}

// cancelledBranches lists the wave's deliberately cancelled branch
// ids, in wave order, for the join envelope.
func cancelledBranches(results []BranchResult) []string {
	var cancelled []string
	for _, res := range results {
		if res.Cancelled {
			cancelled = append(cancelled, res.NodeID)
		}
	}
	return cancelled
}

// startNodeKey carries a per-run entry override on the context.
type startNodeKey struct{}

// WithStartNode overrides where one Execute call begins: the run
// starts at id instead of the definition's entry — the debug/manual
// entry point. Precedence mirrors the legacy runner: a resume
// (agent.Run.ResumeFrom) wins, then this override, then the entry.
// Unknown ids fail validation at Execute.
func WithStartNode(ctx context.Context, id string) context.Context {
	if id == "" || ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, startNodeKey{}, id)
}

// startNodeFromContext returns the WithStartNode override, if any.
func startNodeFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(startNodeKey{}).(string)
	return id, ok && id != ""
}

// stampCheckpoint persists the wave boundary on the host: the full
// completed wave becomes the checkpoint's position marker set, so a
// later resume rebuilds the frontier from every branch's outgoing
// edges — not just the last node's. Checkpoint failures never fail
// the run — durability is the host's choice.
func (g *Graph) stampCheckpoint(ctx context.Context, host agent.Host, run agent.Run, board *agent.Board, wave []string, iterations int, startedAt time.Time) {
	if host == nil {
		return
	}
	// Checkpoint is best-effort by design (durability is the host's
	// choice) but a silent failure here means the host can't resume a
	// crashed run, so surface it as a warning. RunID is included for
	// correlation with the surrounding span.
	if err := host.Checkpoint(ctx, agent.Checkpoint{
		ExecID:            run.RunID,
		Steps:             wave,
		Iteration:         iterations,
		Board:             board.Snapshot(),
		Attributes:        run.Attributes,
		Timestamp:         time.Now(),
		OriginalStartedAt: startedAt,
		SpecVersion:       g.specVersion,
	}); err != nil {
		telemetry.WarnErr(ctx, "graph: checkpoint write failed", err,
			otellog.String(telemetry.AttrGraphName, g.name),
			otellog.String(telemetry.AttrRunID, run.RunID),
			otellog.Int("graph.iteration", iterations))
	}
}

// resolveNext computes the next frontier after a wave: every outgoing
// edge whose condition passes (or is absent) contributes its target;
// END targets terminate quietly.
// resolveNext computes the next frontier after a wave: every outgoing
// edge whose condition passes (or is absent) contributes its target;
// END targets terminate quietly. Conditions evaluate against board
// vars plus the kernel-injected VarIterations (node invocation
// count), so a loop back-edge can soft-exit via "__iterations < 10".
func (g *Graph) resolveNext(board *agent.Board, executed []string, iterations int) ([]string, error) {
	var next []string
	for _, id := range executed {
		for _, e := range g.edges[id] {
			take := true
			if e.Condition != nil {
				ok, err := e.Condition.evaluate(conditionEnv(board, iterations))
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

// conditionEnv assembles the condition evaluation environment: board
// vars plus kernel-injected names. The kernel value wins on collision —
// a user var named "__iterations" cannot fake out a loop's soft exit.
func conditionEnv(board *agent.Board, iterations int) map[string]any {
	env := board.Vars()
	env[VarIterations] = iterations
	return env
}

// classifyContextError converts context termination into a classified
// error. An interrupted cancellation cause is preserved verbatim before
// generic deadline/cancellation classification. nodeID may be empty for
// run-level termination. Returns nil when the context is still alive.
func classifyContextError(ctx context.Context, graphName, nodeID string) error {
	where := fmt.Sprintf("graph %q", graphName)
	if nodeID != "" {
		where = fmt.Sprintf("graph %q node %q", graphName, nodeID)
	}
	if cause := context.Cause(ctx); errdefs.IsInterrupted(cause) {
		return cause
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
