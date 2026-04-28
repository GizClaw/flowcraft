// Package executor provides the graph execution engine.
package executor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type ctxKey int

const ctxKeyActorKey ctxKey = iota

func WithActorKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, ctxKeyActorKey, key)
}

func actorKeyFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyActorKey).(string); ok {
		return v
	}
	return ""
}

var (
	execMeter = telemetry.Meter()

	graphExecCount, _    = execMeter.Int64Counter("graph.executions.total", metric.WithDescription("Total graph executions"))
	graphExecDuration, _ = execMeter.Float64Histogram("graph.duration.seconds", metric.WithDescription("Graph execution duration"))
	nodeExecCount, _     = execMeter.Int64Counter("node.executions.total", metric.WithDescription("Total node executions"))
	nodeExecDuration, _  = execMeter.Float64Histogram("node.duration.seconds", metric.WithDescription("Node execution duration"))
)

const defaultMaxIterations = 200

// Executor is the interface for graph execution engines.
type Executor interface {
	Execute(ctx context.Context, g *graph.Graph, board *graph.Board, opts ...RunOption) (*graph.Board, error)
}

// RunOption configures a single graph execution run.
type RunOption func(*runConfig)

// MergeStrategy defines how parallel branch results are merged.
type MergeStrategy string

const (
	MergeLastWins        MergeStrategy = "last_wins"
	MergeNamespace       MergeStrategy = "namespace"
	MergeErrorOnConflict MergeStrategy = "error_on_conflict"
)

// ParallelConfig configures parallel fork/join execution.
type ParallelConfig struct {
	Enabled       bool          `json:"enabled"`
	MaxBranches   int           `json:"max_branches"`
	MaxNesting    int           `json:"max_nesting"`
	MergeStrategy MergeStrategy `json:"merge_strategy"`
}

// VariableResolver resolves variable references in node configs.
type VariableResolver interface {
	ResolveMap(m map[string]any) (map[string]any, error)
	AddScope(name string, vars map[string]any)
}

// CloneableResolver is an optional interface for resolvers that support cloning
// for use in parallel branches.
type CloneableResolver interface {
	VariableResolver
	Clone() *variable.Resolver
}

type runConfig struct {
	runID          string
	graphName      string
	maxIterations  int
	maxNodeRetries int
	timeout        time.Duration
	startNode      string
	parallel       *ParallelConfig
	resolver       VariableResolver

	// --- event sinks (input options) ---

	// host is the modern event/interrupt/ask sink. Required path
	// going forward; defaulted to engine.NoopHost{} in Execute when
	// the caller doesn't supply WithHost.
	host engine.Host
	// bus is a write-only slot used by the deprecated WithEventBus
	// option. Execute reads it once via resolvePublisher to fold it
	// into publisher; nothing else in the executor consults it.
	//
	// Deprecated: scheduled for removal in v0.3.0 together with
	// WithEventBus. Do NOT add new reads.
	bus event.Bus
	// streamCallback is the legacy per-node delta sink set by
	// WithStreamCallback. newNodePublisher fans every emit to it for
	// v0.2 backwards compatibility; new code should subscribe to the
	// host's event stream instead.
	//
	// Deprecated: scheduled for removal in v0.3.0 together with
	// WithStreamCallback. Do NOT add new reads.
	streamCallback graph.StreamCallback
	// checkpointStore persists a graph-format checkpoint after every
	// node completes. Folded into engine.Host.Checkpointer in v0.3.0;
	// kept here so callers using the deprecated WithCheckpointStore
	// option keep working in the meantime.
	//
	// Deprecated: scheduled for removal in v0.3.0 together with
	// WithCheckpointStore. New code should rely on host.Checkpoint.
	checkpointStore CheckpointStore

	// --- derived (set by Execute) ---

	// publisher is the single event sink consumed by every
	// publishGraph/publishNode and newNodePublisher call. Built once
	// in Execute from host + bus; never read before that.
	publisher engine.Publisher

	nodeLocks *nodeConfigLocks
}

type nodeConfigLocks struct {
	mu sync.Mutex
	m  map[graph.Configurable]*sync.Mutex
}

func WithRunID(id string) RunOption         { return func(c *runConfig) { c.runID = id } }
func WithMaxIterations(n int) RunOption     { return func(c *runConfig) { c.maxIterations = n } }
func WithMaxNodeRetries(n int) RunOption    { return func(c *runConfig) { c.maxNodeRetries = n } }
func WithTimeout(d time.Duration) RunOption { return func(c *runConfig) { c.timeout = d } }
func WithStartNode(id string) RunOption     { return func(c *runConfig) { c.startNode = id } }

// WithCheckpointStore installs a graph-format CheckpointStore that
// persists a checkpoint after every node completes.
//
// Deprecated: graph-format checkpoints will be folded into engine.Host's
// Checkpointer in v0.3.0; new code should rely on host.Checkpoint. The
// store still works in the meantime so existing callers keep functioning.
func WithCheckpointStore(s CheckpointStore) RunOption {
	return func(c *runConfig) { c.checkpointStore = s }
}

func WithParallel(cfg ParallelConfig) RunOption {
	return func(c *runConfig) {
		if cfg.MaxBranches <= 0 {
			cfg.MaxBranches = 10
		}
		if cfg.MaxNesting <= 0 {
			cfg.MaxNesting = 3
		}
		if cfg.MergeStrategy == "" {
			cfg.MergeStrategy = MergeLastWins
		}
		c.parallel = &cfg
	}
}

// WithHost installs the engine.Host the executor will hand to nodes via
// ExecutionContext.Host. When omitted the executor falls back to
// engine.NoopHost{} so nodes can call ctx.Host methods unconditionally.
//
// WithHost subsumes WithEventBus / WithCheckpointStore: when both are
// supplied the host wins for publishing and checkpointing while the bus /
// store remain available for legacy code paths during the transition.
func WithHost(h engine.Host) RunOption {
	return func(c *runConfig) { c.host = h }
}

// WithEventBus installs the event bus used for graph- and node-level
// envelopes.
//
// Deprecated: pass an engine.Host to WithHost instead — the executor now
// publishes envelopes through host.Publish, which lets the host
// centralise routing, fan-out and observability. Scheduled for removal in
// v0.3.0 alongside the other host-overlapping options.
func WithEventBus(bus event.Bus) RunOption {
	return func(c *runConfig) { c.bus = bus }
}

func WithResolver(r VariableResolver) RunOption {
	return func(c *runConfig) { c.resolver = r }
}

// WithStreamCallback installs a legacy stream callback receiving every
// node delta.
//
// Deprecated: subscribe to engine.Host's event bus, or read
// ExecutionContext.Publisher inside a node, instead. Scheduled for
// removal in v0.3.0.
func WithStreamCallback(cb graph.StreamCallback) RunOption {
	return func(c *runConfig) { c.streamCallback = cb }
}

// LocalExecutor is the default single-process executor.
type LocalExecutor struct{}

// NewLocalExecutor creates a new LocalExecutor.
func NewLocalExecutor() *LocalExecutor {
	return &LocalExecutor{}
}

// Execute runs the graph from entry (or startNode) to END.
func (e *LocalExecutor) Execute(ctx context.Context, g *graph.Graph, board *graph.Board, opts ...RunOption) (*graph.Board, error) {
	cfg := runConfig{
		maxIterations: defaultMaxIterations,
		nodeLocks:     &nodeConfigLocks{m: make(map[graph.Configurable]*sync.Mutex)},
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	// cfg.host backs ExecutionContext.Host so nodes can call Host
	// methods (Publish / Interrupts / AskUser / ...) unconditionally.
	if cfg.host == nil {
		cfg.host = engine.NoopHost{}
	}
	cfg.graphName = g.Name()

	// resolvePublisher is the single seam where event sinks are
	// chosen. After this line cfg.bus is invisible to the rest of
	// the executor — every dispatch site reads cfg.publisher.
	cfg.publisher = resolvePublisher(cfg.host, cfg.bus)

	actorKey := actorKeyFrom(ctx)

	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}

	ctx, graphSpan := telemetry.Tracer().Start(ctx, "graph.execute",
		trace.WithAttributes(
			attribute.String("graph.name", g.Name()),
			attribute.String("run.id", cfg.runID),
		),
	)
	defer graphSpan.End()

	graphStart := time.Now()
	telemetry.Info(ctx, "graph execution started",
		otellog.String("graph.name", g.Name()),
		otellog.String("run.id", cfg.runID))

	currentNodes := []string{g.Entry()}
	if cfg.startNode != "" {
		currentNodes = []string{cfg.startNode}
	}

	publishGraphEvent(ctx, cfg.publisher, subjGraphStart(cfg.runID),
		cfg.runID, g.Name(), actorKey, board.Vars())

	iteration := 0
	graphStatus := "success"
	for len(currentNodes) > 0 && iteration < cfg.maxIterations {
		iteration++
		var nextNodes []string

		for _, nodeID := range currentNodes {
			if nodeID == graph.END {
				continue
			}

			node, ok := g.Node(nodeID)
			if !ok {
				return board, fmt.Errorf("node %q not found in graph", nodeID)
			}

			if skip, err := shouldSkip(g, node, board); err != nil {
				return board, err
			} else if skip {
				publishNodeEvent(ctx, cfg.publisher, subjNodeSkipped(cfg.runID, nodeID),
					cfg.runID, g.Name(), actorKey, nodeID, nil)
				resolved, err := resolveNextNodes(g, node, board)
				if err != nil {
					return board, err
				}
				nextNodes = append(nextNodes, resolved...)
				continue
			}

			if cfg.resolver != nil {
				cfg.resolver.AddScope("board", board.Vars())
			}
			var origNodeConfig map[string]any
			if cfgNode, ok := node.(graph.Configurable); ok && cfg.resolver != nil {
				origNodeConfig = cfgNode.Config()
				resolved, err := cfg.resolver.ResolveMap(origNodeConfig)
				if err != nil {
					return board, fmt.Errorf("resolve variables for node %s: %w", nodeID, err)
				}
				cfgNode.SetConfig(resolved)
			}

			if pd, ok := node.(graph.PortDeclarable); ok {
				var nodeCfg map[string]any
				if cfgNode, ok := node.(graph.Configurable); ok {
					nodeCfg = cfgNode.Config()
				}
				if err := graph.ValidateInputsWithConfig(board, pd, nodeCfg); err != nil {
					return board, err
				}
			}

			publishNodeEvent(ctx, cfg.publisher, subjNodeStart(cfg.runID, nodeID),
				cfg.runID, g.Name(), actorKey, nodeID, nil)

			nodeStart := time.Now()
			_, nodeSpan := telemetry.Tracer().Start(ctx, "node."+node.Type()+".execute",
				trace.WithAttributes(
					attribute.String("node.id", nodeID),
					attribute.String("node.type", node.Type()),
					attribute.String("run.id", cfg.runID),
				),
			)

			err := executeWithRetry(ctx, node, board, cfg, nodeID)

			if origNodeConfig != nil {
				node.(graph.Configurable).SetConfig(origNodeConfig)
			}

			nodeDur := time.Since(nodeStart).Seconds()
			nodeExecDuration.Record(ctx, nodeDur,
				metric.WithAttributes(attribute.String("node.type", node.Type())))

			if err != nil {
				nodeSpan.RecordError(err)
				nodeSpan.SetStatus(codes.Error, err.Error())
				nodeSpan.End()

				nodeExecCount.Add(ctx, 1,
					metric.WithAttributes(
						attribute.String("node.type", node.Type()),
						attribute.String("node.id", nodeID),
						attribute.String("status", "error"),
					))

				if errdefs.IsInterrupted(err) {
					board.SetVar(graph.VarInterruptedNode, nodeID)
					publishGraphEvent(ctx, cfg.publisher, subjGraphEnd(cfg.runID),
						cfg.runID, g.Name(), actorKey, nil)
					graphSpan.SetAttributes(attribute.String("graph.status", "interrupted"))
					graphExecCount.Add(ctx, 1,
						metric.WithAttributes(
							attribute.String("graph.name", g.Name()),
							attribute.String("status", "interrupted"),
						))
					graphExecDuration.Record(ctx, time.Since(graphStart).Seconds(),
						metric.WithAttributes(attribute.String("graph.name", g.Name())))
					return board, err
				}

				if ctx.Err() != nil && ctx.Err() == context.DeadlineExceeded {
					return board, errdefs.Timeoutf("node %q execution timed out", nodeID)
				}
				if ctx.Err() != nil {
					return board, errdefs.Abortedf("node %q execution aborted", nodeID)
				}

				telemetry.Error(ctx, "node execution failed",
					otellog.String("node.id", nodeID),
					otellog.String("error", err.Error()))

				publishNodeEvent(ctx, cfg.publisher, subjNodeError(cfg.runID, nodeID),
					cfg.runID, g.Name(), actorKey, nodeID,
					map[string]any{"error": err.Error()})
				return board, err
			}

			nodeSpan.SetStatus(codes.Ok, "OK")
			nodeSpan.End()
			nodeExecCount.Add(ctx, 1,
				metric.WithAttributes(
					attribute.String("node.type", node.Type()),
					attribute.String("node.id", nodeID),
					attribute.String("status", "success"),
				))

			if pd, ok := node.(graph.PortDeclarable); ok {
				if err := graph.ValidateOutputs(board, pd); err != nil {
					return board, err
				}
			}

			publishNodeEvent(ctx, cfg.publisher, subjNodeComplete(cfg.runID, nodeID),
				cfg.runID, g.Name(), actorKey, nodeID,
				map[string]any{"iteration": iteration, "vars": board.Vars()})

			if cfg.checkpointStore != nil {
				if err := cfg.checkpointStore.Save(Checkpoint{
					GraphName: g.Name(),
					RunID:     cfg.runID,
					NodeID:    nodeID,
					Iteration: iteration,
					Board:     board.Snapshot(),
					Timestamp: time.Now(),
				}); err != nil {
					graphSpan.AddEvent("checkpoint save failed",
						trace.WithAttributes(attribute.String("error", err.Error())))
				}
			}

			resolved, err := resolveNextNodes(g, node, board)
			if err != nil {
				return board, err
			}

			if cfg.parallel != nil && len(resolved) > 1 && allUnconditional(g.Edges(nodeID), resolved) {
				joinBoard, err := executeForkJoin(ctx, g, board, resolved, cfg)
				if err != nil {
					return board, err
				}
				board = joinBoard
				joinNode := findJoinNode(g, resolved)
				if joinNode != "" {
					nextNodes = append(nextNodes, joinNode)
				} else {
					telemetry.Warn(ctx, "parallel fork has no join node, branches terminate at __end__",
						otellog.String("graph.name", g.Name()),
						otellog.String("fork_node", nodeID))
				}
				continue
			}

			nextNodes = append(nextNodes, resolved...)
		}

		currentNodes = dedup(nextNodes)

		if len(currentNodes) == 0 || (len(currentNodes) == 1 && currentNodes[0] == graph.END) {
			break
		}

		if ctx.Err() != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return board, errdefs.Timeoutf("graph execution timed out after %d iterations", iteration)
			}
			return board, errdefs.Abortedf("graph execution aborted after %d iterations", iteration)
		}
	}

	if iteration >= cfg.maxIterations && len(currentNodes) > 0 {
		return board, fmt.Errorf("graph execution exceeded max iterations (%d)", cfg.maxIterations)
	}

	publishGraphEvent(ctx, cfg.publisher, subjGraphEnd(cfg.runID),
		cfg.runID, g.Name(), actorKey,
		map[string]any{"iteration": iteration, "vars": board.Vars()})

	graphSpan.SetAttributes(
		attribute.String("graph.status", graphStatus),
		attribute.Int("graph.iterations", iteration),
	)
	graphExecCount.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("graph.name", g.Name()),
			attribute.String("status", graphStatus),
		))
	graphExecDuration.Record(ctx, time.Since(graphStart).Seconds(),
		metric.WithAttributes(attribute.String("graph.name", g.Name())))

	telemetry.Info(ctx, "graph execution completed",
		otellog.String("graph.name", g.Name()),
		otellog.Int("iterations", iteration))

	return board, nil
}

func shouldSkip(g *graph.Graph, node graph.Node, board *graph.Board) (bool, error) {
	compiled, ok := g.SkipCondition(node.ID())
	if !ok {
		return false, nil
	}
	result, err := compiled.Evaluate(board)
	if err != nil {
		return false, fmt.Errorf("skip_condition eval failed for node %s: %w", node.ID(), err)
	}
	return result, nil
}

func resolveNextNodes(g *graph.Graph, node graph.Node, board *graph.Board) ([]string, error) {
	edges := g.Edges(node.ID())
	var unconditional []string
	var matched []string

	for _, edge := range edges {
		if edge.Condition == nil {
			unconditional = append(unconditional, edge.To)
			continue
		}
		ok, err := edge.Condition.Evaluate(board)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, edge.To)
		}
	}

	if len(matched) > 0 {
		return matched, nil
	}
	return unconditional, nil
}

func dedup(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// nodeConfigMu returns a per-node mutex for serializing SetConfig/Execute/
// Restore during parallel branch execution within a single graph run.
func (c *runConfig) nodeConfigMu(node graph.Configurable) *sync.Mutex {
	c.nodeLocks.mu.Lock()
	defer c.nodeLocks.mu.Unlock()
	mu, ok := c.nodeLocks.m[node]
	if !ok {
		mu = &sync.Mutex{}
		c.nodeLocks.m[node] = mu
	}
	return mu
}
