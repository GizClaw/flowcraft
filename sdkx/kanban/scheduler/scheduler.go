// Package scheduler submits kanban tasks on a delay or on a cron
// schedule.
//
// It is deliberately outside sdk/kanban. Submitting a card later is not
// a property of the board — the board's part is a single
// [kanban.Kanban.Submit] call, and everything before it is timekeeping.
// Keeping the two apart means the contract package carries no cron
// dependency, and it leaves room for hosts whose timing comes from
// somewhere sturdier than an in-process timer: a durable queue, a
// database-backed schedule table, a Kubernetes CronJob. This
// implementation is the convenient default, not the only shape.
//
// Everything here goes through kanban's public API, so a Scheduler has
// no privileged access to board internals.
//
// # Durability
//
// Timers and cron entries live in memory. A process restart forgets
// them, so a host that needs schedules to survive supplies a
// [RuleStore]: the Scheduler records each cron rule as it is created,
// removes it when cancelled, and [Scheduler.Restore] re-registers what
// the store holds. Without a store the Scheduler still works, it just
// starts empty.
//
// Nothing here deduplicates across processes. Two replicas with the
// same cron rule fire twice; use one leader, or a store with leases,
// when that matters.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"github.com/robfig/cron/v3"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// MetaScheduleID is the card metadata key under which the Scheduler
// records which rule produced a card. It is exported so dashboards and
// [Overlap] policies can correlate cards with their schedule.
const MetaScheduleID = "schedule_id"

// Rule is a recurring submission: a cron expression plus the task to
// submit each time it fires.
type Rule struct {
	// ID identifies the rule. Empty means the Scheduler assigns one.
	ID string `json:"id"`

	// Cron is a standard five-field cron expression. Descriptors such
	// as "@hourly" and "@every 30m" are also accepted.
	Cron string `json:"cron"`

	// Timezone names the location the expression is evaluated in
	// (IANA, e.g. "Asia/Shanghai"). Empty means UTC.
	Timezone string `json:"timezone,omitempty"`

	// Task is submitted on every trigger.
	Task kanban.Task `json:"task"`

	// Overlap decides what happens when the rule fires while a card
	// from a previous trigger is still outstanding.
	Overlap Overlap `json:"overlap,omitempty"`
}

// Overlap is the policy for a trigger that arrives while the previous
// card from the same rule is still outstanding.
type Overlap string

const (
	// OverlapSkip drops the new trigger, leaving the running card
	// alone. This is the default because a recurring job that falls
	// behind should not pile up work faster than it can be drained.
	OverlapSkip Overlap = ""

	// OverlapAllow submits regardless, so several cards from one rule
	// can be outstanding at once.
	OverlapAllow Overlap = "allow"
)

// RuleStore persists cron rules so they survive a restart. It is the
// host's choice of medium; the Scheduler only needs these three
// operations.
//
// Implementations must be safe for concurrent use.
type RuleStore interface {
	// Save records a rule, overwriting any rule with the same ID.
	Save(ctx context.Context, r Rule) error
	// Delete removes a rule by ID. Removing an absent rule is not an
	// error.
	Delete(ctx context.Context, ruleID string) error
	// List returns every stored rule.
	List(ctx context.Context) ([]Rule, error)
}

// Scheduler submits tasks to a board on a delay or a cron schedule.
type Scheduler struct {
	board *kanban.Kanban
	cron  *cron.Cron
	store RuleStore
	now   func() time.Time

	mu      sync.Mutex
	rules   map[string]cron.EntryID
	timers  map[string]*time.Timer
	started bool
	closed  bool
}

// Option configures a [Scheduler].
type Option func(*Scheduler)

// WithRuleStore makes cron rules durable; see [RuleStore].
func WithRuleStore(s RuleStore) Option {
	return func(sc *Scheduler) { sc.store = s }
}

// New creates a Scheduler bound to board. Call [Scheduler.Start] to
// begin firing and [Scheduler.Close] to stop.
func New(board *kanban.Kanban, opts ...Option) (*Scheduler, error) {
	if board == nil {
		return nil, errdefs.Validationf("scheduler: board is required")
	}
	s := &Scheduler{
		board:  board,
		cron:   cron.New(cron.WithLocation(time.UTC)),
		now:    time.Now,
		rules:  make(map[string]cron.EntryID),
		timers: make(map[string]*time.Timer),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Start begins evaluating cron rules. Delays registered by
// [Scheduler.After] run whether or not Start was called.
func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed {
		return
	}
	s.started = true
	s.cron.Start()
}

// Close stops every cron rule and cancels every pending delay. Cards
// already submitted are untouched — they belong to the board now.
// Close is idempotent.
func (s *Scheduler) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	timers := s.timers
	s.timers = make(map[string]*time.Timer)
	s.rules = make(map[string]cron.EntryID)
	s.mu.Unlock()

	for _, t := range timers {
		t.Stop()
	}
	<-s.cron.Stop().Done()
	return nil
}

// After submits t once, delay from now.
//
// It returns a handle for [Scheduler.CancelDelay], NOT a card id: the
// card does not exist yet and will not until the delay elapses. Watch
// the board for the card itself.
func (s *Scheduler) After(ctx context.Context, delay time.Duration, t kanban.Task) (string, error) {
	if delay < 0 {
		return "", errdefs.Validationf("scheduler: delay must not be negative")
	}
	if t.TargetAgentID == "" {
		return "", errdefs.Validationf("scheduler: Task.TargetAgentID is required")
	}

	handle := "delay-" + newID()
	// Preserve the caller's producer and trace so the eventual card is
	// attributed to whoever asked for it rather than to the timer.
	producer := kanban.ProducerIDFrom(ctx)
	spanCtx := trace.SpanFromContext(ctx).SpanContext()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errdefs.NotAvailablef("scheduler: closed")
	}
	s.timers[handle] = time.AfterFunc(delay, func() {
		s.mu.Lock()
		delete(s.timers, handle)
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		s.fire(s.fireContext(producer, spanCtx), "delay", handle, t, OverlapAllow)
	})
	s.mu.Unlock()
	return handle, nil
}

// CancelDelay cancels a pending delay by the handle [Scheduler.After]
// returned. It reports whether the delay was still pending.
func (s *Scheduler) CancelDelay(handle string) bool {
	s.mu.Lock()
	timer, ok := s.timers[handle]
	delete(s.timers, handle)
	s.mu.Unlock()
	if !ok {
		return false
	}
	return timer.Stop()
}

// Add registers a cron rule and returns its id. When a [RuleStore] is
// configured the rule is persisted before it is armed, so a crash
// cannot leave a rule running that the store does not know about.
func (s *Scheduler) Add(ctx context.Context, r Rule) (string, error) {
	if r.Cron == "" {
		return "", errdefs.Validationf("scheduler: Rule.Cron is required")
	}
	if r.Task.TargetAgentID == "" {
		return "", errdefs.Validationf("scheduler: Rule.Task.TargetAgentID is required")
	}
	if r.ID == "" {
		r.ID = "rule-" + newID()
	}

	if s.store != nil {
		if err := s.store.Save(ctx, r); err != nil {
			return "", fmt.Errorf("scheduler: persist rule %q: %w", r.ID, err)
		}
	}
	if err := s.arm(r); err != nil {
		if s.store != nil {
			_ = s.store.Delete(ctx, r.ID)
		}
		return "", err
	}
	return r.ID, nil
}

// Remove cancels a cron rule and forgets it from the store. It reports
// whether the rule was armed.
func (s *Scheduler) Remove(ctx context.Context, ruleID string) bool {
	s.mu.Lock()
	entry, ok := s.rules[ruleID]
	delete(s.rules, ruleID)
	s.mu.Unlock()
	if ok {
		s.cron.Remove(entry)
	}
	if s.store != nil {
		if err := s.store.Delete(ctx, ruleID); err != nil {
			telemetry.Warn(ctx, "scheduler: failed to delete persisted rule",
				otellog.String("rule_id", ruleID),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
		}
	}
	return ok
}

// Rules lists the ids of every armed rule.
func (s *Scheduler) Rules() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.rules))
	for id := range s.rules {
		out = append(out, id)
	}
	return out
}

// Restore re-arms every rule in the [RuleStore] and returns how many
// were armed. Call it once at startup, before [Scheduler.Start].
//
// A rule the store holds but this build can no longer parse is skipped
// with a warning rather than failing the whole restore: one bad row
// must not stop a process from booting.
func (s *Scheduler) Restore(ctx context.Context) (int, error) {
	if s.store == nil {
		return 0, nil
	}
	rules, err := s.store.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("scheduler: list persisted rules: %w", err)
	}
	armed := 0
	var errs []error
	for _, r := range rules {
		if err := s.arm(r); err != nil {
			errs = append(errs, err)
			telemetry.Warn(ctx, "scheduler: skipping unusable persisted rule",
				otellog.String("rule_id", r.ID),
				otellog.String("cron", r.Cron),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
			continue
		}
		armed++
	}
	return armed, errors.Join(errs...)
}

// arm registers r with the cron engine, replacing any earlier entry
// for the same id.
func (s *Scheduler) arm(r Rule) error {
	spec := r.Cron
	if r.Timezone != "" {
		spec = "CRON_TZ=" + r.Timezone + " " + spec
	}

	rule := r
	entry, err := s.cron.AddFunc(spec, func() {
		s.fire(s.fireContext("scheduler", trace.SpanContext{}),
			"cron", rule.ID, rule.Task, rule.Overlap)
	})
	if err != nil {
		return errdefs.Validationf("scheduler: invalid cron %q: %v", r.Cron, err)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.cron.Remove(entry)
		return errdefs.NotAvailablef("scheduler: closed")
	}
	if previous, ok := s.rules[rule.ID]; ok {
		s.cron.Remove(previous)
	}
	s.rules[rule.ID] = entry
	s.mu.Unlock()
	return nil
}

// fire submits one task, honouring the overlap policy.
func (s *Scheduler) fire(ctx context.Context, kind, id string, t kanban.Task, overlap Overlap) {
	ctx, span := telemetry.Tracer().Start(ctx, "kanban.scheduler."+kind,
		trace.WithAttributes(
			attribute.String("kanban.scheduler.id", id),
			attribute.String(telemetry.AttrKanbanTargetAgentID, t.TargetAgentID),
		))
	defer span.End()

	if overlap == OverlapSkip && s.outstanding(id) {
		telemetry.Info(ctx, "scheduler: skipping trigger, previous card still outstanding",
			otellog.String("rule_id", id))
		return
	}

	card, err := s.board.Submit(ctx, t, kanban.WithMeta(MetaScheduleID, id))
	if err != nil {
		span.RecordError(err)
		telemetry.Warn(ctx, "scheduler: submit failed",
			otellog.String("rule_id", id),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		return
	}
	span.SetAttributes(attribute.String(telemetry.AttrKanbanCardID, card.ID))
}

// outstanding reports whether a card from this rule has yet to reach a
// terminal state. A suspended card counts: it is paused work, not
// finished work, so a fresh trigger would duplicate it.
func (s *Scheduler) outstanding(ruleID string) bool {
	for _, c := range s.board.Query(kanban.Filter{}) {
		if c.Meta[MetaScheduleID] == ruleID && !c.Status.IsTerminal() {
			return true
		}
	}
	return false
}

// fireContext roots a trigger in the board's lifetime so closing the
// board cancels in-flight submissions, while keeping the originating
// trace linked.
func (s *Scheduler) fireContext(producer string, spanCtx trace.SpanContext) context.Context {
	ctx := s.board.Context()
	if spanCtx.IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, spanCtx)
	}
	if producer == "" {
		producer = "scheduler"
	}
	return kanban.WithProducerID(ctx, producer)
}
