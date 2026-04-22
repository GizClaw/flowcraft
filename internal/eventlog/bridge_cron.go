package eventlog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
)

// CronBridge owns the cron.rule.* event lineage.
//
// It performs two duties:
//
//  1. Subscribe to board.Bus() task.submitted events and, when the originating
//     card was produced by the scheduler (card.Producer == "scheduler"),
//     append a cron.rule.fired envelope inside the same Atomic transaction
//     as the cron_fire_idem insert. The idempotency table guarantees that
//     replaying the sdk bus across restarts emits each fired envelope at
//     most once.
//
//  2. Publish cron.rule.created / changed / disabled envelopes on demand.
//     bootstrap calls these helpers from its scheduler.SyncAgent /
//     RemoveAgent code paths so the rule lifecycle is captured in eventlog
//     without sdk knowing anything about cron.rule.* events.
//
// Lifecycle: NewCronBridge -> Attach(ctx, board) -> Close().
type CronBridge struct {
	log   *SQLiteLog
	board *kanban.Board

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewCronBridge constructs a bridge bound to log.
func NewCronBridge(log *SQLiteLog) *CronBridge {
	return &CronBridge{log: log}
}

// Attach subscribes to board.Bus() task.submitted events and starts the
// worker goroutine. Calling Attach twice on the same bridge returns an
// error so the bootstrap path can't accidentally double-publish.
func (b *CronBridge) Attach(parent context.Context, board *kanban.Board) error {
	if board == nil {
		return errors.New("cron bridge: board is nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		return errors.New("cron bridge: already attached")
	}
	ctx, cancel := context.WithCancel(parent)
	bus := board.Bus()
	sub, err := bus.Subscribe(ctx, event.EventFilter{
		Types: []event.EventType{
			event.EventType(kanban.EventTaskSubmitted),
		},
	})
	if err != nil {
		cancel()
		return err
	}
	b.board = board
	b.cancel = cancel
	b.done = make(chan struct{})
	go b.run(ctx, sub)
	return nil
}

// Close cancels the worker and waits for it to drain.
func (b *CronBridge) Close() error {
	b.mu.Lock()
	cancel := b.cancel
	done := b.done
	b.cancel = nil
	b.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	<-done
	return nil
}

func (b *CronBridge) run(ctx context.Context, sub event.Subscription) {
	defer close(b.done)
	defer func() { _ = sub.Close() }()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			b.handleTaskSubmitted(ctx, ev)
		}
	}
}

// handleTaskSubmitted emits a cron.rule.fired envelope when ev was produced
// by the scheduler. Cards submitted by users / agents / api are silently
// ignored.
func (b *CronBridge) handleTaskSubmitted(ctx context.Context, ev event.Event) {
	p, ok := ev.Payload.(kanban.TaskSubmittedPayload)
	if !ok {
		return
	}

	card, found := b.board.GetCardByID(p.CardID)
	if !found {
		// Card has already evicted; nothing to do — cron.rule.fired
		// without an authoritative producer would just be a guess.
		return
	}
	if card.Producer != "scheduler" {
		return
	}
	scheduleID := card.Meta["schedule_id"]
	if scheduleID == "" {
		return
	}

	now := time.Now().UTC()
	scheduledFor := now.Truncate(time.Minute)
	fireKey := buildFireKey(scheduleID, scheduledFor)

	if _, err := b.log.Atomic(ctx, func(uow UnitOfWork) error {
		var seen int
		row := uow.BusinessQueryRow(ctx,
			"SELECT 1 FROM cron_fire_idem WHERE fire_key=?", fireKey)
		switch err := row.Scan(&seen); {
		case err == nil:
			return nil // idempotency hit; envelope already appended once.
		case errors.Is(err, sql.ErrNoRows):
			// fall through and insert.
		default:
			return err
		}
		if _, err := uow.BusinessExec(ctx,
			"INSERT INTO cron_fire_idem(fire_key,rule_id,scheduled_for,ts) VALUES(?,?,?,?)",
			fireKey,
			scheduleID,
			scheduledFor.Format(time.RFC3339Nano),
			Time(),
		); err != nil {
			return err
		}
		return uow.Append(ctx, EnvelopeDraft{
			Partition: PartitionRuntime(p.RuntimeID),
			Type:      "cron.rule.fired",
			Version:   1,
			Category:  "operational",
			Payload: cronFiredPayload{
				RuleID:        scheduleID,
				RuntimeID:     p.RuntimeID,
				FiredAt:       now.Format(time.RFC3339Nano),
				ScheduledFor:  scheduledFor.Format(time.RFC3339Nano),
				TargetAgentID: p.TargetAgentID,
				Query:         p.Query,
				Inputs:        p.Inputs,
				FireKey:       fireKey,
			},
			TraceID: ev.TraceID,
			SpanID:  ev.SpanID,
		})
	}); err != nil {
		slog.Error("cron bridge: append fired failed",
			"rule_id", scheduleID, "err", err)
	}
}

// PublishRuleCreated emits a cron.rule.created envelope. Called by
// bootstrap immediately after kanban.Scheduler.SyncAgent / SyncAgentAppend.
func (b *CronBridge) PublishRuleCreated(ctx context.Context, rule CronRuleEvent) error {
	return b.publishLifecycle(ctx, "cron.rule.created", rule)
}

// PublishRuleChanged emits a cron.rule.changed envelope.
func (b *CronBridge) PublishRuleChanged(ctx context.Context, rule CronRuleEvent) error {
	return b.publishLifecycle(ctx, "cron.rule.changed", rule)
}

// PublishRuleDisabled emits a cron.rule.disabled envelope.
func (b *CronBridge) PublishRuleDisabled(ctx context.Context, rule CronRuleEvent) error {
	return b.publishLifecycle(ctx, "cron.rule.disabled", rule)
}

func (b *CronBridge) publishLifecycle(ctx context.Context, envType string, rule CronRuleEvent) error {
	if rule.RuleID == "" || rule.RuntimeID == "" {
		return errors.New("cron bridge: rule_id and runtime_id required")
	}
	_, err := b.log.Atomic(ctx, func(uow UnitOfWork) error {
		return uow.Append(ctx, EnvelopeDraft{
			Partition: PartitionRuntime(rule.RuntimeID),
			Type:      envType,
			Version:   1,
			Category:  "business",
			Payload: cronLifecyclePayload{
				RuleID:        rule.RuleID,
				RuntimeID:     rule.RuntimeID,
				Expression:    rule.Expression,
				Timezone:      rule.Timezone,
				TargetAgentID: rule.TargetAgentID,
				Query:         rule.Query,
				Inputs:        rule.Inputs,
				Enabled:       rule.Enabled,
				DisabledAt:    rule.DisabledAt,
			},
		})
	})
	return err
}

// CronRuleEvent is the input shape for the lifecycle helpers. Field
// requirements vary by envelope type:
//
//	cron.rule.created  – rule_id, runtime_id, expression, target_agent_id, query
//	cron.rule.changed  – rule_id, runtime_id, plus changed fields
//	cron.rule.disabled – rule_id, runtime_id, disabled_at
type CronRuleEvent struct {
	RuleID        string
	RuntimeID     string
	Expression    string
	Timezone      string
	TargetAgentID string
	Query         string
	Inputs        map[string]any
	Enabled       bool
	DisabledAt    string
}

// cronFiredPayload mirrors §7.2.3 cron.rule.fired with stable JSON keys.
type cronFiredPayload struct {
	RuleID        string         `json:"rule_id"`
	RuntimeID     string         `json:"runtime_id"`
	FiredAt       string         `json:"fired_at"`
	ScheduledFor  string         `json:"scheduled_for"`
	TargetAgentID string         `json:"target_agent_id,omitempty"`
	Query         string         `json:"query,omitempty"`
	Inputs        map[string]any `json:"inputs,omitempty"`
	FireKey       string         `json:"fire_key"`
}

// cronLifecyclePayload covers cron.rule.created / changed / disabled. Empty
// fields are omitted so the on-wire payload stays compact for "changed"
// diffs and "disabled" notifications.
type cronLifecyclePayload struct {
	RuleID        string         `json:"rule_id"`
	RuntimeID     string         `json:"runtime_id"`
	Expression    string         `json:"expression,omitempty"`
	Timezone      string         `json:"timezone,omitempty"`
	TargetAgentID string         `json:"target_agent_id,omitempty"`
	Query         string         `json:"query,omitempty"`
	Inputs        map[string]any `json:"inputs,omitempty"`
	Enabled       bool           `json:"enabled,omitempty"`
	DisabledAt    string         `json:"disabled_at,omitempty"`
}

// buildFireKey derives the idempotency key from rule_id and the bucketed
// scheduled time. We bucket to the minute so cron expressions with second
// precision still dedupe across at-most-once-per-minute restarts.
//
// fire_key = sha256(rule_id || scheduled_for_unix)[:16]
func buildFireKey(ruleID string, scheduledFor time.Time) string {
	h := sha256.New()
	h.Write([]byte(ruleID))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(scheduledFor.Unix(), 10)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}
