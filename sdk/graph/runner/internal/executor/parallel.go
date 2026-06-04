package executor

import (
	"context"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/model"
)

type branchResult struct {
	index    int
	branchID string
	vars     map[string]any
	channels map[string][]model.Message
	err      error
}

type branchParallelController struct {
	parent    *parallelForkController
	branchIdx int
	ctx       context.Context
}

func (c *branchParallelController) CancelNode(nodeID, reason string) bool {
	if c == nil || c.parent == nil || c.ctx.Err() != nil {
		return false
	}
	if c.parent.branchTerminal(c.branchIdx) {
		return false
	}
	return c.parent.cancelNodeFromBranch(c.branchIdx, nodeID, reason)
}

type parallelForkController struct {
	ctx        context.Context
	cfg        runConfig
	graphName  string
	agentID    string
	forkID     string
	forkNodeID string

	mu           sync.Mutex
	branchStarts []string
	cancels      map[int]context.CancelCauseFunc
	nodeBranches map[string][]int
	canceled     map[int]string
	accepted     map[int]struct{}
	abortReason  string
}

func executeForkJoin(ctx context.Context, g *graph.Graph, board *graph.Board, forkNodeID string, branchStarts []string, cfg runConfig) (*graph.Board, error) {
	if cfg.parallel != nil && len(branchStarts) > cfg.parallel.MaxBranches {
		branchStarts = branchStarts[:cfg.parallel.MaxBranches]
	}

	agentID := agentIDFor(ctx, cfg)
	joinNodeID := findJoinNode(g, branchStarts)
	forkID := parallelForkID(cfg.runID, forkNodeID)
	controller := newParallelForkController(ctx, cfg, g.Name(), agentID, forkID, forkNodeID, branchStarts, joinNodeID, g)

	publishGraphEvent(ctx, cfg.publisher, subjParallelFork(cfg.runID),
		cfg.runID, g.Name(), agentID,
		map[string]any{
			"fork_id":    forkID,
			"fork_node":  forkNodeID,
			"branch_ids": branchStarts,
			"join_node":  joinNodeID,
		})

	results := make([]branchResult, len(branchStarts))
	var wg sync.WaitGroup

	snapshot := board.Snapshot()
	branchCtxs := make([]context.Context, len(branchStarts))
	for i, startID := range branchStarts {
		branchCtx, cancelBranch := context.WithCancelCause(ctx)
		controller.attachBranch(i, cancelBranch)
		branchCtx = graph.WithParallelController(branchCtx, &branchParallelController{
			parent:    controller,
			branchIdx: i,
			ctx:       branchCtx,
		})
		branchCtx = withParallelBranchInfo(branchCtx, parallelBranchInfo{
			ForkID:      forkID,
			BranchID:    startID,
			Speculative: true,
		})
		branchCtxs[i] = branchCtx
	}
	defer controller.releaseBranchContexts()

	for i, startID := range branchStarts {
		wg.Add(1)
		go func(idx int, sid string, branchCtx context.Context) {
			defer wg.Done()
			branchBoard := graph.RestoreBoard(snapshot)
			branchCfg := cfg
			if cr, ok := cfg.resolver.(CloneableResolver); ok {
				branchCfg.resolver = cr.Clone()
			}
			err := runBranch(branchCtx, g, branchBoard, sid, joinNodeID, branchCfg)
			results[idx] = branchResult{
				index:    idx,
				branchID: sid,
				vars:     branchBoard.Vars(),
				channels: branchBoard.ChannelsCopy(),
				err:      err,
			}
			if err != nil && !controller.branchCanceled(idx) {
				controller.abortUnaccepted(fmt.Sprintf("fork aborted: parallel branch %q failed: %v", sid, err))
			}
		}(i, startID, branchCtxs[i])
	}

	wg.Wait()

	if reason, ok := controller.abortState(); ok {
		primaryIdx := primaryErrorIndex(results)
		if primaryIdx >= 0 {
			return board, fmt.Errorf("parallel branch %q failed: %w",
				branchStarts[primaryIdx], results[primaryIdx].err)
		}
		return board, errdefs.Abortedf("%s", reason)
	}

	primaryIdx := -1
	for i, r := range results {
		if controller.branchCanceled(i) {
			continue
		}
		if r.err == nil {
			continue
		}
		if primaryIdx == -1 {
			primaryIdx = i
		}
		if !errdefs.Is(r.err, context.Canceled) {
			primaryIdx = i
			break
		}
	}
	if primaryIdx >= 0 {
		err := fmt.Errorf("parallel branch %q failed: %w",
			branchStarts[primaryIdx], results[primaryIdx].err)
		controller.cancelUnaccepted("fork aborted: " + err.Error())
		return board, err
	}

	mergeResults := make([]branchResult, 0, len(results))
	acceptedIdxs := make([]int, 0, len(results))
	for i, r := range results {
		if !controller.branchCanceled(i) {
			mergeResults = append(mergeResults, r)
			acceptedIdxs = append(acceptedIdxs, i)
		}
	}

	strategy := MergeLastWins
	if cfg.parallel != nil {
		strategy = cfg.parallel.MergeStrategy
	}
	mergeFn := lookupMerge(strategy)
	if err := mergeFn(board, snapshot, mergeResults); err != nil {
		controller.cancelUnaccepted("fork aborted: merge failed: " + err.Error())
		return board, err
	}

	controller.acceptBranches(acceptedIdxs)

	publishGraphEvent(ctx, cfg.publisher, subjParallelJoin(cfg.runID),
		cfg.runID, g.Name(), agentID,
		map[string]any{
			"fork_id":    forkID,
			"branch_ids": branchStarts,
			"vars":       board.Vars(),
		})

	return board, nil
}

func parallelForkID(runID, forkNodeID string) string {
	if runID == "" {
		return forkNodeID
	}
	return runID + ":" + forkNodeID
}

func newParallelForkController(
	ctx context.Context,
	cfg runConfig,
	graphName, agentID, forkID, forkNodeID string,
	branchStarts []string,
	joinNodeID string,
	g *graph.Graph,
) *parallelForkController {
	return &parallelForkController{
		ctx:          ctx,
		cfg:          cfg,
		graphName:    graphName,
		agentID:      agentID,
		forkID:       forkID,
		forkNodeID:   forkNodeID,
		branchStarts: append([]string(nil), branchStarts...),
		cancels:      make(map[int]context.CancelCauseFunc, len(branchStarts)),
		nodeBranches: branchNodeIndex(g, branchStarts, joinNodeID),
		canceled:     make(map[int]string),
		accepted:     make(map[int]struct{}),
	}
}

func (c *parallelForkController) attachBranch(idx int, cancel context.CancelCauseFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancels[idx] = cancel
}

func (c *parallelForkController) cancelNodeFromBranch(callerIdx int, nodeID, reason string) bool {
	if reason == "" {
		reason = "cancelled by parallel.cancelNode"
	}

	type cancellation struct {
		branchID string
		cancel   context.CancelCauseFunc
	}

	c.mu.Lock()
	if _, ok := c.accepted[callerIdx]; ok {
		c.mu.Unlock()
		return false
	}
	if _, ok := c.canceled[callerIdx]; ok {
		c.mu.Unlock()
		return false
	}
	branchIdxs := append([]int(nil), c.nodeBranches[nodeID]...)
	cancellations := make([]cancellation, 0, len(branchIdxs))
	for _, idx := range branchIdxs {
		if _, ok := c.accepted[idx]; ok {
			continue
		}
		if _, ok := c.canceled[idx]; ok {
			continue
		}
		c.canceled[idx] = reason
		cancellations = append(cancellations, cancellation{
			branchID: c.branchStarts[idx],
			cancel:   c.cancels[idx],
		})
	}
	c.mu.Unlock()

	for _, item := range cancellations {
		if item.cancel != nil {
			item.cancel(fmt.Errorf("parallel branch %q canceled: %s", item.branchID, reason))
		}
		c.publishBranchCancel(item.branchID, nodeID, reason)
	}
	return len(cancellations) > 0
}

func (c *parallelForkController) branchTerminal(idx int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.canceled[idx]; ok {
		return true
	}
	_, ok := c.accepted[idx]
	return ok
}

func (c *parallelForkController) branchCanceled(idx int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.canceled[idx]
	return ok
}

func (c *parallelForkController) cancelBranchContexts(cause error) {
	c.mu.Lock()
	cancels := make([]context.CancelCauseFunc, 0, len(c.cancels))
	for _, cancel := range c.cancels {
		cancels = append(cancels, cancel)
	}
	c.mu.Unlock()

	for _, cancel := range cancels {
		cancel(cause)
	}
}

func (c *parallelForkController) releaseBranchContexts() {
	c.cancelBranchContexts(nil)
}

func (c *parallelForkController) abortUnaccepted(reason string) {
	c.mu.Lock()
	if c.abortReason == "" {
		c.abortReason = reason
	}
	reason = c.abortReason
	c.mu.Unlock()
	c.cancelUnaccepted(reason)
}

func (c *parallelForkController) abortState() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.abortReason, c.abortReason != ""
}

func (c *parallelForkController) cancelUnaccepted(reason string) {
	type cancellation struct {
		branchID string
		cancel   context.CancelCauseFunc
	}

	c.mu.Lock()
	cancellations := make([]cancellation, 0, len(c.branchStarts))
	for idx, branchID := range c.branchStarts {
		if _, ok := c.accepted[idx]; ok {
			continue
		}
		if _, ok := c.canceled[idx]; ok {
			continue
		}
		c.canceled[idx] = reason
		cancellations = append(cancellations, cancellation{
			branchID: branchID,
			cancel:   c.cancels[idx],
		})
	}
	c.mu.Unlock()

	for _, item := range cancellations {
		if item.cancel != nil {
			item.cancel(fmt.Errorf("parallel branch %q canceled: %s", item.branchID, reason))
		}
		c.publishBranchCancel(item.branchID, "", reason)
	}
}

func (c *parallelForkController) acceptBranches(idxs []int) {
	branchIDs := make([]string, 0, len(idxs))

	c.mu.Lock()
	for _, idx := range idxs {
		if _, ok := c.canceled[idx]; ok {
			continue
		}
		if _, ok := c.accepted[idx]; ok {
			continue
		}
		c.accepted[idx] = struct{}{}
		branchIDs = append(branchIDs, c.branchStarts[idx])
	}
	c.mu.Unlock()

	for _, branchID := range branchIDs {
		c.publishBranchAccept(branchID)
	}
}

func (c *parallelForkController) publishBranchAccept(branchID string) {
	payload := map[string]any{
		"fork_id":   c.forkID,
		"branch_id": branchID,
	}
	publishGraphEvent(c.ctx, c.cfg.publisher, subjParallelBranchAccept(c.cfg.runID),
		c.cfg.runID, c.graphName, c.agentID, payload)
	c.publishBranchControlDelta(branchID, engine.StreamDeltaParallelBranchAccept, payload)
}

func (c *parallelForkController) publishBranchCancel(branchID, nodeID, reason string) {
	payload := map[string]any{
		"fork_id":   c.forkID,
		"fork_node": c.forkNodeID,
		"branch_id": branchID,
		"reason":    reason,
	}
	if nodeID != "" {
		payload["node_id"] = nodeID
	}
	publishGraphEvent(c.ctx, c.cfg.publisher, subjParallelBranchCancel(c.cfg.runID),
		c.cfg.runID, c.graphName, c.agentID, payload)
	c.publishBranchControlDelta(branchID, engine.StreamDeltaParallelBranchCancel, payload)
}

func (c *parallelForkController) publishBranchControlDelta(branchID string, eventType engine.StreamDeltaType, payload map[string]any) {
	if c.cfg.publisher == nil {
		return
	}
	delta := map[string]any{
		"type":        eventType,
		"fork_id":     c.forkID,
		"branch_id":   branchID,
		"speculative": true,
	}
	for k, v := range payload {
		delta[k] = v
	}
	publishNodeEvent(c.ctx, c.cfg.publisher,
		engine.SubjectStreamDelta(c.cfg.runID, stepActorFor(c.agentID, branchID)),
		c.cfg.runID, c.graphName, c.agentID, branchID, delta)
}

func branchNodeIndex(g *graph.Graph, branchStartIDs []string, joinNodeID string) map[string][]int {
	succs := make(map[string][]string)
	for _, e := range g.AllEdges() {
		succs[e.From] = append(succs[e.From], e.To)
	}

	out := make(map[string][]int)
	for i, startID := range branchStartIDs {
		for nodeID := range bfsReachableUntilJoin(succs, startID, joinNodeID) {
			out[nodeID] = append(out[nodeID], i)
		}
	}
	return out
}

func bfsReachableUntilJoin(succs map[string][]string, start, joinNodeID string) map[string]bool {
	reached := make(map[string]bool)
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if reached[cur] || cur == graph.END || (joinNodeID != "" && cur == joinNodeID) {
			continue
		}
		reached[cur] = true
		queue = append(queue, succs[cur]...)
	}
	return reached
}

func primaryErrorIndex(results []branchResult) int {
	primaryIdx := -1
	for i, r := range results {
		if r.err == nil {
			continue
		}
		if primaryIdx == -1 {
			primaryIdx = i
		}
		if !errdefs.Is(r.err, context.Canceled) {
			return i
		}
	}
	return primaryIdx
}

func runBranch(ctx context.Context, g *graph.Graph, board *graph.Board, startID, joinID string, cfg runConfig) error {
	currentID := startID
	for currentID != joinID && currentID != graph.END {
		if err := ctx.Err(); err != nil {
			return errdefs.FromContext(err)
		}

		node, ok := g.Node(currentID)
		if !ok {
			return errdefs.NotFoundf("branch node %q not found", currentID)
		}

		if skip, err := shouldSkip(g, node, board); err != nil {
			return err
		} else if skip {
			resolved, err := resolveNextNodes(g, node, board)
			if err != nil {
				return fmt.Errorf("branch routing from %q: %w", currentID, err)
			}
			if len(resolved) == 0 {
				break
			}
			currentID = resolved[0]
			continue
		}

		if cfg.resolver != nil {
			cfg.resolver.AddScope("board", board.Vars())
		}

		var execErr error
		if cfgNode, ok := node.(graph.Configurable); ok && cfg.resolver != nil {
			origConfig := cfgNode.Config()
			resolved, err := cfg.resolver.ResolveMap(origConfig)
			if err != nil {
				return fmt.Errorf("resolve variables for branch node %s: %w", currentID, err)
			}
			cfg.nodeConfigMu(cfgNode).Lock()
			cfgNode.SetConfig(resolved)
			execErr = executeWithRetry(ctx, node, board, cfg, currentID)
			cfgNode.SetConfig(origConfig)
			cfg.nodeConfigMu(cfgNode).Unlock()
		} else {
			execErr = executeWithRetry(ctx, node, board, cfg, currentID)
		}

		if execErr != nil {
			return fmt.Errorf("branch node %q failed: %w", currentID, execErr)
		}

		resolved, err := resolveNextNodes(g, node, board)
		if err != nil {
			return fmt.Errorf("branch routing from %q: %w", currentID, err)
		}
		if len(resolved) == 0 {
			break
		}
		currentID = resolved[0]
	}
	return nil
}

func findJoinNode(g *graph.Graph, branchStartIDs []string) string {
	if len(branchStartIDs) == 0 {
		return ""
	}

	succs := make(map[string][]string)
	for _, e := range g.AllEdges() {
		succs[e.From] = append(succs[e.From], e.To)
	}

	startSet := make(map[string]bool, len(branchStartIDs))
	for _, id := range branchStartIDs {
		startSet[id] = true
	}

	reachSets := make([]map[string]bool, len(branchStartIDs))
	for i, sid := range branchStartIDs {
		reachSets[i] = bfsReachable(succs, sid)
	}

	type entry struct {
		id   string
		dist int
	}
	visited := make(map[string]bool)
	queue := []entry{{branchStartIDs[0], 0}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur.id] {
			continue
		}
		visited[cur.id] = true

		if !startSet[cur.id] {
			allReach := true
			for j := 1; j < len(reachSets); j++ {
				if !reachSets[j][cur.id] {
					allReach = false
					break
				}
			}
			if allReach {
				return cur.id
			}
		}

		for _, next := range succs[cur.id] {
			if next != graph.END && !visited[next] {
				queue = append(queue, entry{next, cur.dist + 1})
			}
		}
	}
	return ""
}

func bfsReachable(succs map[string][]string, start string) map[string]bool {
	reached := make(map[string]bool)
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if reached[cur] || cur == graph.END {
			continue
		}
		reached[cur] = true
		queue = append(queue, succs[cur]...)
	}
	return reached
}

func allUnconditional(edges []graph.Edge, targets []string) bool {
	targetSet := make(map[string]bool, len(targets))
	for _, t := range targets {
		targetSet[t] = true
	}
	for _, e := range edges {
		if targetSet[e.To] && e.Condition != nil {
			return false
		}
	}
	return true
}
