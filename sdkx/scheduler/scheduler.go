// Package scheduler provides type-safe in-process delay and cron scheduling.
//
// The package owns timekeeping only. Dispatching work and deciding whether
// previously dispatched work is still outstanding are supplied by adapters.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"github.com/robfig/cron/v3"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

const (
	// MetaScheduleID is the metadata key adapters should use to correlate
	// dispatched work with the schedule that produced it.
	MetaScheduleID = "schedule_id"

	// AttrScheduleID identifies a schedule in telemetry.
	AttrScheduleID = "scheduler.schedule.id"
	// AttrScheduleKind identifies whether a trigger is a delay or cron rule.
	AttrScheduleKind = "scheduler.schedule.kind"
)

// Dispatcher submits one typed value for execution and returns the business
// execution whose lifecycle an overlap policy may inspect.
type Dispatcher[T any] interface {
	Dispatch(ctx context.Context, scheduleID string, value T) (Outstanding, error)
}

// Outstanding represents one dispatched business execution. IsOutstanding
// must describe that execution's lifecycle, not callback submission.
type Outstanding interface {
	IsOutstanding(ctx context.Context) (bool, error)
}

// Rule is a recurring typed dispatch.
type Rule[T any] struct {
	ID       string  `json:"id"`
	Cron     string  `json:"cron"`
	Timezone string  `json:"timezone,omitempty"`
	Value    T       `json:"value"`
	Overlap  Overlap `json:"overlap,omitempty"`
}

// Overlap controls a trigger arriving while prior business work is outstanding.
type Overlap string

const (
	// OverlapSkip suppresses a trigger while prior work remains outstanding.
	OverlapSkip Overlap = ""
	// OverlapAllow dispatches every trigger.
	OverlapAllow Overlap = "allow"
)

// RuleStore persists recurring rules. Implementations must be concurrency-safe.
type RuleStore[T any] interface {
	Save(ctx context.Context, rule Rule[T]) error
	Delete(ctx context.Context, ruleID string) error
	List(ctx context.Context) ([]Rule[T], error)
}

// Option configures a Scheduler.
type Option[T any] func(*Scheduler[T])

// WithRuleStore enables persistence and Restore.
func WithRuleStore[T any](store RuleStore[T]) Option[T] {
	return func(s *Scheduler[T]) {
		s.store = store
		s.storeSet = true
	}
}

// WithValueValidator validates rule values before Add persists them and before
// Restore arms persisted rules.
func WithValueValidator[T any](validate func(T) error) Option[T] {
	return func(s *Scheduler[T]) {
		s.valueValidator = validate
	}
}

type ruleState[T any] struct {
	entry      cron.EntryID
	rule       Rule[T]
	generation uint64

	gateMu sync.Mutex
	firing bool
}

// Scheduler dispatches typed values after delays or on cron schedules.
type Scheduler[T any] struct {
	dispatcher     Dispatcher[T]
	cron           *cron.Cron
	store          RuleStore[T]
	storeSet       bool
	valueValidator func(T) error

	mu          sync.Mutex
	opsMu       sync.Mutex
	rules       map[string]*ruleState[T]
	timers      map[string]*time.Timer
	outstanding map[string][]Outstanding
	callbacks   sync.WaitGroup
	closeDone   chan struct{}
	started     bool
	closed      bool
}

// New constructs a scheduler. Start begins cron evaluation; delays are armed
// immediately.
func New[T any](dispatcher Dispatcher[T], opts ...Option[T]) (*Scheduler[T], error) {
	if isNil(dispatcher) {
		return nil, errdefs.Validationf("scheduler: dispatcher is required")
	}
	s := &Scheduler[T]{
		dispatcher:  dispatcher,
		cron:        cron.New(cron.WithLocation(time.UTC)),
		rules:       make(map[string]*ruleState[T]),
		timers:      make(map[string]*time.Timer),
		outstanding: make(map[string][]Outstanding),
		closeDone:   make(chan struct{}),
	}
	for _, option := range opts {
		if option != nil {
			option(s)
		}
	}
	if s.storeSet && isNil(s.store) {
		return nil, errdefs.Validationf("scheduler: rule store must not be nil")
	}
	return s, nil
}

// Start begins evaluating cron rules. It is safe to call repeatedly.
func (s *Scheduler[T]) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed {
		return
	}
	s.started = true
	s.cron.Start()
}

// Close stops cron evaluation and pending delays. It is idempotent.
func (s *Scheduler[T]) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		return nil
	}
	s.closed = true
	timers := s.timers
	s.timers = make(map[string]*time.Timer)
	s.rules = make(map[string]*ruleState[T])
	s.outstanding = make(map[string][]Outstanding)
	s.mu.Unlock()

	for _, timer := range timers {
		timer.Stop()
	}
	<-s.cron.Stop().Done()
	s.callbacks.Wait()
	close(s.closeDone)
	return nil
}

// After dispatches value once after delay and returns a cancellation handle.
func (s *Scheduler[T]) After(ctx context.Context, delay time.Duration, value T) (string, error) {
	if delay < 0 {
		return "", errdefs.Validationf("scheduler: delay must not be negative")
	}
	handle := "delay-" + newID()
	fireCtx := triggerContext(ctx)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errdefs.NotAvailablef("scheduler: closed")
	}
	s.timers[handle] = time.AfterFunc(delay, func() {
		if !s.admitDelay(handle) {
			return
		}
		defer s.callbacks.Done()
		s.fire(fireCtx, "delay", handle, value, OverlapAllow, nil, 0)
	})
	s.mu.Unlock()
	return handle, nil
}

// CancelDelay cancels a still-pending delay.
func (s *Scheduler[T]) CancelDelay(handle string) bool {
	s.mu.Lock()
	timer, ok := s.timers[handle]
	delete(s.timers, handle)
	s.mu.Unlock()
	return ok && timer.Stop()
}

// Add persists and arms a recurring rule.
func (s *Scheduler[T]) Add(ctx context.Context, rule Rule[T]) (string, error) {
	if rule.ID == "" {
		rule.ID = "rule-" + newID()
	}
	spec, err := s.validateRule(rule, false)
	if err != nil {
		return "", err
	}

	s.opsMu.Lock()
	defer s.opsMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errdefs.NotAvailablef("scheduler: closed")
	}
	state := s.rules[rule.ID]
	var oldRule Rule[T]
	hadOld := state != nil
	if hadOld {
		oldRule = state.rule
	} else {
		state = &ruleState[T]{}
	}
	generation := state.generation + 1
	s.mu.Unlock()

	if s.store != nil {
		if err := s.store.Save(ctx, rule); err != nil {
			return "", fmt.Errorf("scheduler: persist rule %q: %w", rule.ID, err)
		}
	}
	if err := s.arm(rule, spec, state, generation); err != nil {
		return "", errors.Join(err, s.rollbackRule(ctx, rule.ID, oldRule, hadOld))
	}
	return rule.ID, nil
}

// Remove disarms and deletes a recurring rule.
// The bool reports whether an armed rule existed; persistence failures are
// returned even though the in-memory rule has already been disarmed.
func (s *Scheduler[T]) Remove(ctx context.Context, ruleID string) (bool, error) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()

	s.mu.Lock()
	state, ok := s.rules[ruleID]
	delete(s.rules, ruleID)
	delete(s.outstanding, ruleID)
	s.mu.Unlock()
	if ok {
		s.cron.Remove(state.entry)
	}
	if s.store != nil {
		if err := s.store.Delete(ctx, ruleID); err != nil {
			return ok, fmt.Errorf("scheduler: delete persisted rule %q: %w", ruleID, err)
		}
	}
	return ok, nil
}

// Rules returns armed rule IDs in stable order.
func (s *Scheduler[T]) Rules() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.rules))
	for id := range s.rules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Restore re-arms persisted rules. Invalid rows are skipped and joined into
// the returned error so one bad row does not prevent startup.
func (s *Scheduler[T]) Restore(ctx context.Context) (int, error) {
	if s.store == nil {
		return 0, nil
	}
	s.opsMu.Lock()
	defer s.opsMu.Unlock()

	rules, err := s.store.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("scheduler: list persisted rules: %w", err)
	}
	var errs []error
	armed := 0
	for _, rule := range rules {
		spec, err := s.validateRule(rule, true)
		if err == nil {
			s.mu.Lock()
			state := s.rules[rule.ID]
			if state == nil {
				state = &ruleState[T]{}
			}
			generation := state.generation + 1
			s.mu.Unlock()
			err = s.arm(rule, spec, state, generation)
		}
		if err != nil {
			errs = append(errs, err)
			telemetry.Warn(ctx, "scheduler: skipping unusable persisted rule",
				otellog.String("rule_id", rule.ID),
				otellog.String("cron", rule.Cron),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
			continue
		}
		armed++
	}
	return armed, errors.Join(errs...)
}

func (s *Scheduler[T]) validateRule(rule Rule[T], persisted bool) (string, error) {
	if rule.ID == "" && persisted {
		return "", errdefs.Validationf("scheduler: persisted Rule.ID is required")
	}
	if rule.Cron == "" {
		return "", errdefs.Validationf("scheduler: Rule.Cron is required")
	}
	if rule.Overlap != OverlapSkip && rule.Overlap != OverlapAllow {
		return "", errdefs.Validationf("scheduler: unsupported overlap policy %q", rule.Overlap)
	}
	spec := rule.Cron
	if rule.Timezone != "" {
		spec = "CRON_TZ=" + rule.Timezone + " " + spec
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	if _, err := parser.Parse(spec); err != nil {
		return "", errdefs.Validationf("scheduler: invalid cron %q: %v", rule.Cron, err)
	}
	if s.valueValidator != nil {
		if err := s.valueValidator(rule.Value); err != nil {
			return "", fmt.Errorf("scheduler: invalid Rule.Value: %w", err)
		}
	}
	return spec, nil
}

func (s *Scheduler[T]) arm(rule Rule[T], spec string, state *ruleState[T], generation uint64) error {
	entry, err := s.cron.AddFunc(spec, func() {
		if !s.admitRule(rule.ID, state, generation) {
			return
		}
		defer s.callbacks.Done()
		s.fire(context.Background(), "cron", rule.ID, rule.Value, rule.Overlap, state, generation)
	})
	if err != nil {
		return errdefs.Validationf("scheduler: invalid cron %q: %v", rule.Cron, err)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.cron.Remove(entry)
		return errdefs.NotAvailablef("scheduler: closed")
	}
	previous := state.entry
	state.entry = entry
	state.rule = rule
	state.generation = generation
	s.rules[rule.ID] = state
	s.mu.Unlock()
	if previous != 0 {
		s.cron.Remove(previous)
	}
	return nil
}

func (s *Scheduler[T]) rollbackRule(ctx context.Context, id string, old Rule[T], hadOld bool) error {
	if s.store == nil {
		return nil
	}
	if hadOld {
		if err := s.store.Save(ctx, old); err != nil {
			return fmt.Errorf("scheduler: restore persisted rule %q after arm failure: %w", id, err)
		}
		return nil
	}
	if err := s.store.Delete(ctx, id); err != nil {
		return fmt.Errorf("scheduler: roll back persisted rule %q after arm failure: %w", id, err)
	}
	return nil
}

func (s *Scheduler[T]) admitDelay(handle string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		delete(s.timers, handle)
		return false
	}
	if _, ok := s.timers[handle]; !ok {
		return false
	}
	delete(s.timers, handle)
	s.callbacks.Add(1)
	return true
}

func (s *Scheduler[T]) admitRule(id string, state *ruleState[T], generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.rules[id] != state || state.generation != generation {
		return false
	}
	s.callbacks.Add(1)
	return true
}

func (s *Scheduler[T]) fire(ctx context.Context, kind, id string, value T, overlap Overlap, state *ruleState[T], generation uint64) {
	ctx, span := telemetry.TracerWithSuffix("scheduler").Start(ctx, "scheduler."+kind,
		trace.WithAttributes(
			attribute.String(AttrScheduleID, id),
			attribute.String(AttrScheduleKind, kind),
		))
	defer span.End()

	if overlap == OverlapSkip {
		state.gateMu.Lock()
		if state.firing {
			state.gateMu.Unlock()
			return
		}
		state.firing = true
		state.gateMu.Unlock()
		defer func() {
			state.gateMu.Lock()
			state.firing = false
			state.gateMu.Unlock()
		}()
		if !s.ruleIsCurrent(id, state, generation) {
			return
		}
		pending, err := s.hasOutstanding(ctx, id)
		if err != nil {
			span.RecordError(err)
			telemetry.Warn(ctx, "scheduler: outstanding check failed; skipping trigger",
				otellog.String("rule_id", id),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
			return
		}
		if pending {
			telemetry.Info(ctx, "scheduler: skipping trigger, previous work still outstanding",
				otellog.String("rule_id", id))
			return
		}
		if !s.ruleIsCurrent(id, state, generation) {
			return
		}
	}
	work, err := s.dispatcher.Dispatch(ctx, id, value)
	if err != nil {
		span.RecordError(err)
		telemetry.Warn(ctx, "scheduler: dispatch failed",
			otellog.String("rule_id", id),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		return
	}
	if overlap == OverlapSkip {
		if isNil(work) {
			err := errdefs.Internalf("scheduler: dispatcher returned nil outstanding work")
			span.RecordError(err)
			telemetry.Warn(ctx, "scheduler: dispatch returned no outstanding handle",
				otellog.String("rule_id", id),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
			return
		}
		s.mu.Lock()
		if s.rules[id] == state {
			s.outstanding[id] = append(s.outstanding[id], work)
		}
		s.mu.Unlock()
	}
}

func (s *Scheduler[T]) ruleIsCurrent(id string, state *ruleState[T], generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && s.rules[id] == state && state.generation == generation
}

func (s *Scheduler[T]) hasOutstanding(ctx context.Context, scheduleID string) (bool, error) {
	s.mu.Lock()
	work := slices.Clone(s.outstanding[scheduleID])
	s.mu.Unlock()

	remaining := work[:0]
	for index, item := range work {
		pending, err := item.IsOutstanding(ctx)
		if err != nil {
			return false, err
		}
		if pending {
			remaining = append(remaining, item)
			remaining = append(remaining, work[index+1:]...)
			s.replaceOutstanding(scheduleID, remaining)
			return true, nil
		}
	}
	s.replaceOutstanding(scheduleID, remaining)
	return false, nil
}

func (s *Scheduler[T]) replaceOutstanding(scheduleID string, remaining []Outstanding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.rules[scheduleID] == nil {
		return
	}
	if len(remaining) == 0 {
		delete(s.outstanding, scheduleID)
		return
	}
	s.outstanding[scheduleID] = slices.Clone(remaining)
}

func triggerContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	spanCtx := trace.SpanFromContext(ctx).SpanContext()
	if spanCtx.IsValid() {
		return trace.ContextWithSpanContext(context.Background(), spanCtx)
	}
	return context.Background()
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
