package executor

import (
	"context"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/model"
)

type branchResult struct {
	vars     map[string]any
	channels map[string][]model.Message
	err      error
}

func executeForkJoin(ctx context.Context, g *graph.Graph, board *graph.Board, branchStarts []string, cfg runConfig) (*graph.Board, error) {
	if cfg.parallel != nil && len(branchStarts) > cfg.parallel.MaxBranches {
		branchStarts = branchStarts[:cfg.parallel.MaxBranches]
	}

	actorKey := actorKeyFrom(ctx)
	joinNodeID := findJoinNode(g, branchStarts)

	publishGraphEvent(ctx, cfg.publisher, subjParallelFork(cfg.runID),
		cfg.runID, g.Name(), actorKey,
		map[string]any{
			"branch_ids": branchStarts,
			"join_node":  joinNodeID,
		})

	results := make([]branchResult, len(branchStarts))
	var wg sync.WaitGroup

	branchCtx, cancelBranches := context.WithCancel(ctx)
	defer cancelBranches()

	snapshot := board.Snapshot()

	for i, startID := range branchStarts {
		wg.Add(1)
		go func(idx int, sid string) {
			defer wg.Done()
			branchBoard := graph.RestoreBoard(snapshot)
			branchCfg := cfg
			if cr, ok := cfg.resolver.(CloneableResolver); ok {
				branchCfg.resolver = cr.Clone()
			}
			err := runBranch(branchCtx, g, branchBoard, sid, joinNodeID, branchCfg)
			results[idx] = branchResult{
				vars:     branchBoard.Vars(),
				channels: branchBoard.ChannelsCopy(),
				err:      err,
			}
			if err != nil {
				cancelBranches()
			}
		}(i, startID)
	}

	wg.Wait()

	primaryIdx := -1
	for i, r := range results {
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
		return board, fmt.Errorf("parallel branch %q failed: %w",
			branchStarts[primaryIdx], results[primaryIdx].err)
	}

	strategy := MergeLastWins
	if cfg.parallel != nil {
		strategy = cfg.parallel.MergeStrategy
	}
	mergeFn := lookupMerge(strategy)
	if err := mergeFn(board, snapshot, results); err != nil {
		return board, err
	}

	publishGraphEvent(ctx, cfg.publisher, subjParallelJoin(cfg.runID),
		cfg.runID, g.Name(), actorKey,
		map[string]any{
			"branch_ids": branchStarts,
			"vars":       board.Vars(),
		})

	return board, nil
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
