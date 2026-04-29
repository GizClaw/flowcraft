package kanban

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"github.com/robfig/cron/v3"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

const cardTypeCronRule = "cron_rule"

// CronJob describes a cron-based recurring execution rule.
// Defined in the kanban package so that sdk/kanban does not depend on model.
// bootstrap converts model.Schedule → CronJob.
type CronJob struct {
	ID       string
	Cron     string
	Query    string
	Enabled  *bool
	Timezone string
	Source   string // "static" or "dynamic"
}

func (j CronJob) isEnabled() bool {
	return j.Enabled == nil || *j.Enabled
}

// cronRulePayload is the payload stored in a cron_rule card on the board.
type cronRulePayload struct {
	AgentID    string `json:"agent_id"`
	ScheduleID string `json:"schedule_id"`
	Cron       string `json:"cron"`
	Query      string `json:"query"`
	Timezone   string `json:"timezone,omitempty"`
}

// Scheduler provides cron-based and delay-based task scheduling on top of
// Kanban. It is an optional component embedded inside a Kanban instance.
type Scheduler struct {
	kanban *Kanban
	cron   *cron.Cron

	mu      sync.RWMutex
	entries map[string][]entryRecord // agentID → cron entries
	timers  map[string]*time.Timer   // placeholderID → delay timer
	closed  bool
}

type entryRecord struct {
	scheduleID string
	cronID     cron.EntryID
	source     string // "static" or "dynamic"
}

// NewScheduler creates a Scheduler. Pass it to kanban.New via WithScheduler;
// the Kanban reference is wired automatically.
func NewScheduler() *Scheduler {
	return &Scheduler{
		cron:    cron.New(cron.WithLocation(time.UTC)),
		entries: make(map[string][]entryRecord),
		timers:  make(map[string]*time.Timer),
	}
}

// Start begins cron scheduling.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop cancels all pending timers and waits for running cron jobs to finish.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	s.closed = true
	for _, t := range s.timers {
		t.Stop()
	}
	s.timers = make(map[string]*time.Timer)
	s.mu.Unlock()
	stopCtx := s.cron.Stop()
	<-stopCtx.Done()
}

func (s *Scheduler) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// SyncAgent loads or replaces static cron rules for an agent.
// Dynamic entries (source="dynamic") are preserved across syncs.
func (s *Scheduler) SyncAgent(agentID string, jobs []CronJob) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var kept []entryRecord
	for _, rec := range s.entries[agentID] {
		if rec.source == "dynamic" {
			kept = append(kept, rec)
		} else {
			s.cron.Remove(rec.cronID)
		}
	}
	s.entries[agentID] = kept

	for _, job := range jobs {
		if !job.isEnabled() || job.Cron == "" || job.Query == "" {
			continue
		}

		aid := agentID
		j := job
		cronExpr := j.Cron
		if j.Timezone != "" {
			cronExpr = "CRON_TZ=" + j.Timezone + " " + cronExpr
		}

		cronID, err := s.cron.AddFunc(cronExpr, func() {
			s.fire(aid, j.ID, j.Query)
		})
		if err != nil {
			telemetry.Warn(context.Background(), "kanban.scheduler: invalid cron expression",
				otellog.String("agent_id", aid),
				otellog.String("cron", j.Cron),
				otellog.String("error", err.Error()))
			continue
		}
		s.entries[agentID] = append(s.entries[agentID], entryRecord{
			scheduleID: j.ID,
			cronID:     cronID,
			source:     j.Source,
		})
	}
}

// RemoveAgent clears all cron rules for an agent. An EventCronRuleDisabled
// event is published for each removed schedule so external observers can keep
// in sync without polling.
func (s *Scheduler) RemoveAgent(agentID string) {
	s.mu.Lock()
	removed := make([]string, 0, len(s.entries[agentID]))
	for _, rec := range s.entries[agentID] {
		s.cron.Remove(rec.cronID)
		removed = append(removed, rec.scheduleID)
	}
	delete(s.entries, agentID)
	s.mu.Unlock()

	if s.kanban != nil {
		ctx := context.Background()
		if s.kanban.ctx != nil {
			ctx = s.kanban.ctx
		}
		bus := s.kanban.board.Bus()
		scopeID := s.kanban.board.ScopeID()
		for _, scheduleID := range removed {
			publishCronEvent(ctx, bus, EventCronRuleDisabled, scheduleID, scopeID, CronRuleDisabledPayload{
				ScheduleID: scheduleID,
				AgentID:    agentID,
			})
		}
	}
}

// LoadFromBoard scans the board for active cron_rule cards and registers them.
// Called once during bootstrap after the board has been restored from store.
func (s *Scheduler) LoadFromBoard() int {
	if s.kanban == nil {
		return 0
	}
	count := 0
	for _, card := range s.kanban.QueryCards(CardFilter{Type: cardTypeCronRule}) {
		if card.Status != CardPending {
			continue
		}
		p, ok := parseCronRulePayload(card.Payload)
		if !ok || p.Cron == "" || p.Query == "" || p.AgentID == "" {
			continue
		}
		s.SyncAgentAppend(p.AgentID, CronJob{
			ID:       p.ScheduleID,
			Cron:     p.Cron,
			Query:    p.Query,
			Timezone: p.Timezone,
			Source:   "dynamic",
		})
		count++
	}
	return count
}

// SyncAgentAppend adds a single cron job to an agent without clearing existing entries.
func (s *Scheduler) SyncAgentAppend(agentID string, job CronJob) {
	if !job.isEnabled() || job.Cron == "" || job.Query == "" {
		return
	}

	aid := agentID
	j := job
	cronExpr := j.Cron
	if j.Timezone != "" {
		cronExpr = "CRON_TZ=" + j.Timezone + " " + cronExpr
	}

	cronID, err := s.cron.AddFunc(cronExpr, func() {
		s.fire(aid, j.ID, j.Query)
	})
	if err != nil {
		telemetry.Warn(context.Background(), "kanban.scheduler: invalid cron expression",
			otellog.String("agent_id", aid),
			otellog.String("cron", j.Cron),
			otellog.String("error", err.Error()))
		return
	}

	s.mu.Lock()
	s.entries[agentID] = append(s.entries[agentID], entryRecord{
		scheduleID: j.ID,
		cronID:     cronID,
		source:     j.Source,
	})
	s.mu.Unlock()
}

// schedulerCtx returns the base context used for all scheduler-originated
// submissions. Trace span + producer ID are attached so downstream telemetry
// can correlate cron / delayed tasks back to the scheduler that fired them.
func (s *Scheduler) schedulerCtx(parent context.Context, agentID, scheduleID, kind string) (context.Context, trace.Span) {
	if parent == nil {
		parent = context.Background()
	}
	if s.kanban != nil && s.kanban.ctx != nil {
		// Use the Kanban lifecycle ctx so a Kanban.Stop() also cancels in-flight
		// scheduler submits. We keep the parent's trace baggage by deriving the
		// span from the parent ctx if it had one.
		if sc := trace.SpanFromContext(parent).SpanContext(); sc.IsValid() {
			parent = trace.ContextWithSpanContext(s.kanban.ctx, sc)
		} else {
			parent = s.kanban.ctx
		}
	}
	ctx, span := telemetry.Tracer().Start(parent, "kanban.scheduler."+kind,
		trace.WithAttributes(
			attribute.String("kanban.scheduler.agent_id", agentID),
			attribute.String("kanban.scheduler.schedule_id", scheduleID),
		),
	)
	ctx = WithProducerID(ctx, "scheduler")
	return ctx, span
}

// fire submits a scheduled task card, skipping if a previous card is still active.
// It propagates a fresh scheduler-rooted trace span so submitted tasks correlate
// back to the cron rule that triggered them, instead of starting from a bare
// context.Background().
func (s *Scheduler) fire(agentID, scheduleID, query string) {
	if s.isClosed() || s.kanban == nil {
		return
	}

	ctx, span := s.schedulerCtx(nil, agentID, scheduleID, "fire")
	defer span.End()

	for _, card := range s.kanban.QueryCards(CardFilter{Type: "task"}) {
		if card.Meta["schedule_id"] == scheduleID &&
			(card.Status == CardPending || card.Status == CardClaimed) {
			telemetry.Info(ctx, "kanban.scheduler: skipping, previous task still active",
				otellog.String("agent_id", agentID),
				otellog.String("schedule_id", scheduleID),
				otellog.String("card_id", card.ID))
			return
		}
	}

	cardID, err := s.kanban.Submit(ctx, TaskOptions{
		TargetAgentID: agentID,
		Query:         query,
		AgentID:       agentID,
		Meta:          map[string]string{"schedule_id": scheduleID},
	})
	if err != nil {
		span.RecordError(err)
		telemetry.Warn(ctx, "kanban.scheduler: submit failed",
			otellog.String("agent_id", agentID),
			otellog.String("schedule_id", scheduleID),
			otellog.String("error", err.Error()))
		return
	}
	span.SetAttributes(attribute.String("kanban.scheduler.card_id", cardID))

	publishCronEvent(ctx, s.kanban.board.Bus(), EventCronRuleFired, scheduleID, s.kanban.board.ScopeID(), CronRuleFiredPayload{
		ScheduleID: scheduleID,
		AgentID:    agentID,
		CardID:     cardID,
		Query:      query,
	})

	telemetry.Info(ctx, "kanban.scheduler: task submitted",
		otellog.String("agent_id", agentID),
		otellog.String("schedule_id", scheduleID),
		otellog.String("card_id", cardID))
}

// scheduleSubmit handles TaskOptions with Delay or Cron fields.
// Called from Kanban.Submit when scheduling fields are present.
func (s *Scheduler) scheduleSubmit(ctx context.Context, opts TaskOptions) (string, error) {
	delay := opts.Delay
	cronExpr := opts.Cron
	tz := opts.Timezone

	opts.Delay = ""
	opts.Cron = ""
	opts.Timezone = ""

	if delay != "" {
		return s.submitWithDelay(ctx, opts, delay)
	}
	if cronExpr != "" {
		return s.submitWithCron(ctx, opts, cronExpr, tz)
	}
	return s.kanban.Submit(ctx, opts)
}

// submitWithDelay registers a one-shot timer that fires opts after delayStr.
// The original caller's producer ID and trace span context are captured at
// submit time and re-attached when the timer fires, so rate-limit accounting
// and trace correlation behave the same as a synchronous Submit.
func (s *Scheduler) submitWithDelay(ctx context.Context, opts TaskOptions, delayStr string) (string, error) {
	d, err := time.ParseDuration(delayStr)
	if err != nil {
		return "", fmt.Errorf("kanban.scheduler: invalid delay %q: %w", delayStr, err)
	}

	placeholderID := schedGenID()

	if ctx == nil {
		ctx = context.Background()
	}
	originalProducer := ProducerIDFrom(ctx)
	originalSpanCtx := trace.SpanFromContext(ctx).SpanContext()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errdefs.NotAvailablef("kanban.scheduler: stopped")
	}
	timer := time.AfterFunc(d, func() {
		s.mu.Lock()
		delete(s.timers, placeholderID)
		s.mu.Unlock()
		if s.isClosed() || s.kanban == nil {
			return
		}

		base := s.kanban.ctx
		if base == nil {
			base = context.Background()
		}
		if originalSpanCtx.IsValid() {
			base = trace.ContextWithSpanContext(base, originalSpanCtx)
		}
		producer := originalProducer
		if producer == "" {
			producer = "scheduler"
		}
		fireCtx, span := telemetry.Tracer().Start(base, "kanban.scheduler.delay.fire",
			trace.WithAttributes(
				attribute.String("kanban.scheduler.placeholder_id", placeholderID),
				attribute.String("kanban.scheduler.original_producer", originalProducer),
			),
		)
		fireCtx = WithProducerID(fireCtx, producer)
		_, err := s.kanban.Submit(fireCtx, opts)
		if err != nil {
			span.RecordError(err)
			telemetry.Warn(fireCtx, "kanban.scheduler: delayed submit failed",
				otellog.String("placeholder_id", placeholderID),
				otellog.String("error", err.Error()))
		}
		span.End()
	})
	s.timers[placeholderID] = timer
	s.mu.Unlock()

	return placeholderID, nil
}

func schedGenID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// submitWithCron registers a dynamic cron job and persists it as a cron_rule card.
// The caller's ctx (producer ID + trace span) is preserved on the
// EventCronRuleCreated event so external observers can correlate the rule
// back to the user request that created it.
func (s *Scheduler) submitWithCron(ctx context.Context, opts TaskOptions, cronExpr, tz string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	scheduleID := schedGenID()
	agentID := opts.TargetAgentID
	query := opts.Query

	rawCron := cronExpr
	if tz != "" {
		cronExpr = "CRON_TZ=" + tz + " " + cronExpr
	}

	cronID, err := s.cron.AddFunc(cronExpr, func() {
		s.fire(agentID, scheduleID, query)
	})
	if err != nil {
		return "", fmt.Errorf("kanban.scheduler: invalid cron %q: %w", cronExpr, err)
	}

	if s.kanban != nil {
		s.kanban.board.Produce(cardTypeCronRule, "scheduler", cronRulePayload{
			AgentID:    agentID,
			ScheduleID: scheduleID,
			Cron:       rawCron,
			Query:      query,
			Timezone:   tz,
		}, WithMeta("schedule_id", scheduleID), WithMeta("agent_id", agentID))

		publishCronEvent(ctx, s.kanban.board.Bus(), EventCronRuleCreated, scheduleID, s.kanban.board.ScopeID(), CronRuleCreatedPayload{
			ScheduleID: scheduleID,
			AgentID:    agentID,
			Cron:       rawCron,
			Query:      query,
			Timezone:   tz,
		})
	}

	s.mu.Lock()
	s.entries[agentID] = append(s.entries[agentID], entryRecord{
		scheduleID: scheduleID,
		cronID:     cronID,
		source:     "dynamic",
	})
	s.mu.Unlock()

	return scheduleID, nil
}

// parseCronRulePayload extracts cronRulePayload from a card's Payload.
func parseCronRulePayload(payload any) (cronRulePayload, bool) {
	m, ok := payload.(map[string]any)
	if !ok {
		return cronRulePayload{}, false
	}
	p := cronRulePayload{}
	p.AgentID, _ = m["agent_id"].(string)
	p.ScheduleID, _ = m["schedule_id"].(string)
	p.Cron, _ = m["cron"].(string)
	p.Query, _ = m["query"].(string)
	p.Timezone, _ = m["timezone"].(string)
	return p, p.AgentID != "" && p.ScheduleID != ""
}
