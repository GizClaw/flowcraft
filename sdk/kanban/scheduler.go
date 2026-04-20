package kanban

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"github.com/robfig/cron/v3"
	otellog "go.opentelemetry.io/otel/log"
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

// SetKanban injects the owning Kanban reference.
//
// Deprecated: Use WithScheduler when constructing a Kanban via New instead.
// New automatically wires the scheduler's Kanban reference. Removed in v0.2.0.
func (s *Scheduler) SetKanban(k *Kanban) { s.kanban = k }

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

// RemoveAgent clears all cron rules for an agent.
func (s *Scheduler) RemoveAgent(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.entries[agentID] {
		s.cron.Remove(rec.cronID)
	}
	delete(s.entries, agentID)
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

// fire submits a scheduled task card, skipping if a previous card is still active.
func (s *Scheduler) fire(agentID, scheduleID, query string) {
	if s.isClosed() || s.kanban == nil {
		return
	}

	for _, card := range s.kanban.QueryCards(CardFilter{Type: "task"}) {
		if card.Meta["schedule_id"] == scheduleID &&
			(card.Status == CardPending || card.Status == CardClaimed) {
			telemetry.Info(context.Background(), "kanban.scheduler: skipping, previous task still active",
				otellog.String("agent_id", agentID),
				otellog.String("schedule_id", scheduleID),
				otellog.String("card_id", card.ID))
			return
		}
	}

	ctx := WithProducerID(context.Background(), "scheduler")
	cardID, err := s.kanban.Submit(ctx, TaskOptions{
		TargetAgentID: agentID,
		Query:         query,
		AgentID:       agentID,
		Meta:          map[string]string{"schedule_id": scheduleID},
	})
	if err != nil {
		telemetry.Warn(context.Background(), "kanban.scheduler: submit failed",
			otellog.String("agent_id", agentID),
			otellog.String("schedule_id", scheduleID),
			otellog.String("error", err.Error()))
		return
	}
	telemetry.Info(context.Background(), "kanban.scheduler: task submitted",
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

func (s *Scheduler) submitWithDelay(_ context.Context, opts TaskOptions, delayStr string) (string, error) {
	d, err := time.ParseDuration(delayStr)
	if err != nil {
		return "", fmt.Errorf("kanban.scheduler: invalid delay %q: %w", delayStr, err)
	}

	placeholderID := schedGenID()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", fmt.Errorf("kanban.scheduler: stopped")
	}
	timer := time.AfterFunc(d, func() {
		s.mu.Lock()
		delete(s.timers, placeholderID)
		s.mu.Unlock()
		if !s.isClosed() && s.kanban != nil {
			_, _ = s.kanban.Submit(context.Background(), opts)
		}
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
func (s *Scheduler) submitWithCron(_ context.Context, opts TaskOptions, cronExpr, tz string) (string, error) {
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
