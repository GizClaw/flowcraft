package kanban

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

type ctxKey int

const (
	ctxKeyProducerID ctxKey = iota
	ctxKeyKanban
	ctxKeyTaskBoard
)

// WithProducerID injects the producer ID (e.g. agent ID) into the context.
func WithProducerID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyProducerID, id)
}

// ProducerIDFrom returns the producer ID injected via WithProducerID.
func ProducerIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyProducerID).(string); ok {
		return v
	}
	return ""
}

// TaskPayload is the payload of a task card.
type TaskPayload struct {
	TargetAgentID string         `json:"target_agent_id"`
	Query         string         `json:"query"`
	Inputs        map[string]any `json:"inputs,omitempty"`
	UserQuery     string         `json:"user_query,omitempty"`
	DispatchNote  string         `json:"dispatch_note,omitempty"`
}

// ResultPayload is the payload of a result card.
type ResultPayload struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// TaskOptions configures a task submission.
type TaskOptions struct {
	TargetAgentID string            `json:"target_agent_id"`
	Query         string            `json:"query"`
	Inputs        map[string]any    `json:"inputs,omitempty"`
	AgentID       string            `json:"agent_id,omitempty"`
	Timeout       time.Duration     `json:"timeout,omitempty"`
	UserQuery     string            `json:"user_query,omitempty"`
	DispatchNote  string            `json:"dispatch_note,omitempty"`
	Delay         string            `json:"delay,omitempty"`
	Cron          string            `json:"cron,omitempty"`
	Timezone      string            `json:"timezone,omitempty"`
	Meta          map[string]string `json:"meta,omitempty"`
}

// KanbanConfig configures the Kanban system.
type KanbanConfig struct {
	MaxPendingTasks int `json:"max_pending_tasks,omitempty"`
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() KanbanConfig {
	return KanbanConfig{
		MaxPendingTasks: 100,
	}
}

// DefaultStopTimeout bounds how long Kanban.Stop() waits for in-flight
// executors before logging and returning. Override with WithStopTimeout(0)
// for the legacy "wait forever" behaviour, or with a different positive
// duration. Callers building integrations on top of Kanban (SIGTERM
// handlers, test harnesses) rely on Stop terminating in bounded time.
const DefaultStopTimeout = 10 * time.Second

// Kanban coordinates multi-agent collaboration via a shared board.
//
// Kanban does not own an independent event bus: every state transition is
// published on Board.Bus(), which is the single source of truth.
// (k *Kanban).Bus() is a thin alias for board.Bus() kept for ergonomics.
type Kanban struct {
	board       *Board
	executor    AgentExecutor
	validator   AgentValidator
	cfg         KanbanConfig
	metrics     *kanbanMetrics
	scheduler   *Scheduler
	stopTimeout time.Duration
	ctx         context.Context
	cancel      context.CancelFunc

	mu       sync.RWMutex
	closed   bool
	stopOnce sync.Once
	execWg   sync.WaitGroup
}

// New creates a Kanban instance.
func New(ctx context.Context, board *Board, opts ...Option) *Kanban {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	k := &Kanban{
		board:       board,
		cfg:         DefaultConfig(),
		metrics:     newKanbanMetrics(ctx),
		stopTimeout: DefaultStopTimeout,
		ctx:         runCtx,
		cancel:      cancel,
	}
	for _, opt := range opts {
		opt(k)
	}
	if k.scheduler != nil {
		k.scheduler.kanban = k
	}
	return k
}

// AgentValidator checks whether a target agent ID is valid before submission.
// Return nil if valid; return an error (ideally listing available IDs) if not.
type AgentValidator func(ctx context.Context, agentID string) error

// Option configures a Kanban instance.
type Option func(*Kanban)

// WithAgentExecutor sets the executor for running tasks on target agents.
func WithAgentExecutor(e AgentExecutor) Option {
	return func(k *Kanban) { k.executor = e }
}

// WithAgentValidator sets a pre-submit validator for target agent IDs.
func WithAgentValidator(v AgentValidator) Option {
	return func(k *Kanban) { k.validator = v }
}

// WithEventBus is retained for source compatibility only and has no effect.
//
// Deprecated: Board.Bus() is the single source of truth for every Kanban
// state transition (task submitted / claimed / completed / failed + cron
// rule created / fired / disabled). Pass your bus to the Board instead, or
// subscribe to board.Bus() and forward as needed. This option will be
// removed in v0.2.0.
func WithEventBus(_ event.LegacyEventBus) Option {
	return func(*Kanban) {}
}

// WithConfig sets the Kanban configuration.
func WithConfig(cfg KanbanConfig) Option {
	return func(k *Kanban) { k.cfg = cfg }
}

// WithScheduler attaches a Scheduler to the Kanban instance.
func WithScheduler(s *Scheduler) Option {
	return func(k *Kanban) { k.scheduler = s }
}

// WithStopTimeout bounds how long Stop will wait for in-flight executors.
// The default is DefaultStopTimeout (10s); pass 0 or a negative value to
// fall back to the legacy "wait forever" behaviour.
//
// On timeout, Stop logs the unfinished goroutine count captured at start
// of Stop and returns; the executors keep running but no longer block
// process exit. AgentExecutor implementations should always honour ctx
// cancellation — using context.WithoutCancel inside an executor chain
// defeats this safety net.
func WithStopTimeout(d time.Duration) Option {
	return func(k *Kanban) { k.stopTimeout = d }
}

// Scheduler returns the embedded scheduler, or nil.
func (k *Kanban) Scheduler() *Scheduler { return k.scheduler }

// Stop closes the Kanban admission path and waits for in-flight executors.
// The wait is bounded by the stop timeout (DefaultStopTimeout = 10s, or
// the value passed to WithStopTimeout); set WithStopTimeout(0) to wait
// forever. On timeout the alive-goroutine count captured at start of Stop
// is logged and Stop returns without waiting for stuck executors. The
// board is always closed before Stop returns.
func (k *Kanban) Stop() {
	k.stopOnce.Do(func() {
		k.mu.Lock()
		k.closed = true
		k.mu.Unlock()
		if k.scheduler != nil {
			k.scheduler.Stop()
		}
		if k.cancel != nil {
			k.cancel()
		}
		defer func() {
			if k.board != nil {
				k.board.Close()
			}
		}()

		if k.stopTimeout <= 0 {
			k.execWg.Wait()
			return
		}

		goroutinesAtStop := runtime.NumGoroutine()
		done := make(chan struct{})
		go func() {
			k.execWg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(k.stopTimeout):
			ctx := k.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			telemetry.Warn(ctx, "kanban: Stop timed out waiting for executors; returning early",
				otellog.String("stop_timeout", k.stopTimeout.String()),
				otellog.Int("goroutines_at_stop", goroutinesAtStop),
				otellog.Int("goroutines_now", runtime.NumGoroutine()))
		}
	})
}

func (k *Kanban) isClosed() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.closed
}

func (k *Kanban) executionContext(producer string) context.Context {
	ctx := k.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if producer != "" {
		ctx = WithProducerID(ctx, producer)
	}
	return ctx
}

// Board returns the owned task board.
func (k *Kanban) Board() *Board { return k.board }

// Bus returns the event bus that publishes every Kanban state transition.
// It is an alias for Board().Bus(); both return the same underlying bus.
func (k *Kanban) Bus() event.LegacyEventBus { return k.board.Bus() }

// Submit produces a task card on the board and executes via AgentExecutor.
// All tasks are asynchronous; results are delivered via callback.
func (k *Kanban) Submit(ctx context.Context, opts TaskOptions) (string, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "kanban.submit",
		trace.WithAttributes(
			attribute.String("kanban.target_agent_id", opts.TargetAgentID),
		),
	)
	defer span.End()

	if k.validator != nil {
		if err := k.validator(ctx, opts.TargetAgentID); err != nil {
			span.RecordError(err)
			return "", err
		}
	}

	if (opts.Delay != "" || opts.Cron != "") && k.scheduler != nil {
		return k.scheduler.scheduleSubmit(ctx, opts)
	}

	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return "", errdefs.NotAvailablef("kanban is stopped")
	}
	if k.cfg.MaxPendingTasks > 0 {
		pendingCount := k.board.CountByStatus(CardPending, "task")
		if pendingCount >= k.cfg.MaxPendingTasks {
			k.mu.Unlock()
			return "", errdefs.RateLimitf("pending task limit reached (%d)", k.cfg.MaxPendingTasks)
		}
	}

	producer := ProducerIDFrom(ctx)

	payload := TaskPayload{
		TargetAgentID: opts.TargetAgentID,
		Query:         opts.Query,
		Inputs:        opts.Inputs,
		UserQuery:     opts.UserQuery,
		DispatchNote:  opts.DispatchNote,
	}

	var cardOpts []CardOption
	if opts.AgentID != "" {
		cardOpts = append(cardOpts, WithConsumer(opts.AgentID))
	}
	for mk, mv := range opts.Meta {
		cardOpts = append(cardOpts, WithMeta(mk, mv))
	}

	card := k.board.Produce("task", producer, payload, cardOpts...)
	shouldExecute := false
	if k.executor != nil {
		k.execWg.Add(1)
		shouldExecute = true
	}
	k.mu.Unlock()

	k.metrics.incTasksSubmitted(ctx, attribute.String("target_agent_id", opts.TargetAgentID))
	// EventTaskSubmitted is published by Board.Produce → publishProduceEvent.
	// Do not re-publish here; Board.Bus() is the single source of truth.

	if shouldExecute {
		go func() {
			defer k.execWg.Done()
			execCtx := k.executionContext(producer)
			k.metrics.addAgentsActive(execCtx, 1)
			startedAt := time.Now()
			defer func() {
				k.metrics.addAgentsActive(execCtx, -1)
			}()
			if err := k.executor.ExecuteTask(execCtx, k.board.ScopeID(), opts.TargetAgentID, card, opts.Query, opts.Inputs); err != nil {
				elapsed := time.Since(startedAt)
				k.metrics.recordTaskDuration(execCtx, elapsed.Seconds(),
					attribute.String("target_agent_id", opts.TargetAgentID),
					attribute.String("status", "error"),
				)
				// Board.Fail publishes EventTaskFailed via publishCardEvent;
				// no need to emit a second copy from here.
				k.board.Fail(card.ID, err.Error())
				telemetry.Warn(ctx, "kanban executor failed",
					otellog.String("card", card.ID),
					otellog.String("error", err.Error()))
				return
			}
			k.metrics.recordTaskDuration(execCtx, time.Since(startedAt).Seconds(),
				attribute.String("target_agent_id", opts.TargetAgentID),
				attribute.String("status", "success"),
			)
		}()
	}

	return card.ID, nil
}

// QueryCards queries cards on the board by filter.
func (k *Kanban) QueryCards(filter CardFilter) []*Card {
	return k.board.Query(filter)
}

// GetCard returns the card with the given ID, or an error if not found.
func (k *Kanban) GetCard(_ context.Context, cardID string) (*Card, error) {
	c, ok := k.board.GetCardByID(cardID)
	if !ok {
		return nil, errdefs.NotFoundf("card %q not found", cardID)
	}
	return c, nil
}

// Broadcast produces a signal card visible to all agents.
func (k *Kanban) Broadcast(ctx context.Context, signalType string, payload any) {
	if k.isClosed() {
		return
	}
	producer := ProducerIDFrom(ctx)
	k.board.Produce("signal", producer, payload, WithMeta("signal_type", signalType))
}
