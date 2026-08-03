// Package memory adapts memory maintenance operations to the generic
// sdkx/scheduler runtime.
package memory

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdkx/memory/config"
	corescheduler "github.com/GizClaw/flowcraft/sdkx/scheduler"
)

const (
	// CompactRuleID is the stable ID of the lifecycle compact rule.
	CompactRuleID = "memory-compact"
	// ArchiveRuleID is the stable ID of the lifecycle archive rule.
	ArchiveRuleID = "memory-archive"
)

// Operation identifies a memory maintenance operation.
type Operation string

const (
	OperationCompact Operation = "compact"
	OperationArchive Operation = "archive"
)

// Task is the typed value dispatched by the generic scheduler.
type Task struct {
	Operation   Operation     `json:"operation"`
	OlderThan   time.Duration `json:"older_than"`
	Keep        int           `json:"keep,omitempty"`
	Destination string        `json:"destination,omitempty"`
}

type (
	// LifecycleSpec is the deploy configuration accepted by New.
	LifecycleSpec = config.LifecycleSpec
	// Rule is a recurring memory maintenance task.
	Rule = corescheduler.Rule[Task]
	// RuleStore persists memory scheduling rules.
	RuleStore = corescheduler.RuleStore[Task]
)

// Option configures a Scheduler.
type Option func(*options)

type options struct {
	clock    sdkmemory.Clock
	store    RuleStore
	storeSet bool
}

// WithClock overrides the runtime clock used to calculate retention cutoffs.
func WithClock(clock sdkmemory.Clock) Option {
	return func(options *options) {
		options.clock = clock
	}
}

// WithRuleStore enables persistence for rules added after construction and
// enables Restore. A store cannot be combined with lifecycle rules in New,
// because constructors do not perform persistence I/O; add those rules later
// with Add and an explicit context.
func WithRuleStore(store RuleStore) Option {
	return func(options *options) {
		options.store = store
		options.storeSet = true
	}
}

// Scheduler schedules synchronous, process-local memory maintenance.
type Scheduler struct {
	core       *corescheduler.Scheduler[Task]
	dispatcher *adapter
}

// New constructs a memory scheduler and registers each enabled lifecycle block.
// Empty blocks are disabled. Lifecycle rules use stable IDs and OverlapSkip.
func New(rt *sdkmemory.Runtime, lifecycle LifecycleSpec, opts ...Option) (*Scheduler, error) {
	if rt == nil {
		return nil, errdefs.Validationf("memory scheduler: runtime is required")
	}
	if err := lifecycle.Validate(); err != nil {
		return nil, errdefs.Validation(err)
	}

	settings := options{clock: rt.Spec().Clock}
	for _, option := range opts {
		if option != nil {
			option(&settings)
		}
	}
	if isNil(settings.clock) {
		return nil, errdefs.Validationf("memory scheduler: clock must not be nil")
	}
	if settings.storeSet && isNil(settings.store) {
		return nil, errdefs.Validationf("memory scheduler: rule store must not be nil")
	}
	rules := lifecycleRules(lifecycle)
	if settings.store != nil && len(rules) != 0 {
		return nil, errdefs.Validationf(
			"memory scheduler: lifecycle rules cannot be persisted during New; use an empty lifecycle and Add with an explicit context")
	}

	lifecycleCtx, cancel := context.WithCancel(context.Background())
	scope := rt.Spec().DefaultScope
	if scope.IsZero() {
		scope = sdkmemory.Scope{RuntimeID: rt.Spec().RuntimeID}
	}
	dispatcher := &adapter{
		runtime: rt,
		scope:   scope,
		clock:   settings.clock,
		ctx:     lifecycleCtx,
		cancel:  cancel,
		serial:  make(chan struct{}, 1),
	}
	var coreOptions []corescheduler.Option[Task]
	coreOptions = append(coreOptions, corescheduler.WithValueValidator(Task.validate))
	if settings.store != nil {
		coreOptions = append(coreOptions, corescheduler.WithRuleStore(settings.store))
	}
	core, err := corescheduler.New[Task](dispatcher, coreOptions...)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &Scheduler{core: core, dispatcher: dispatcher}
	for _, rule := range rules {
		if _, err := s.Add(context.Background(), rule); err != nil {
			cancel()
			_ = core.Close()
			return nil, err
		}
	}
	return s, nil
}

// Start begins cron evaluation.
func (s *Scheduler) Start() {
	if s != nil && s.core != nil {
		s.core.Start()
	}
}

// Close first cancels memory I/O and then waits for scheduler callbacks.
func (s *Scheduler) Close() error {
	if s == nil {
		return nil
	}
	if s.dispatcher != nil {
		s.dispatcher.close()
	}
	if s.core == nil {
		return nil
	}
	return s.core.Close()
}

// Add validates and registers a recurring memory maintenance task.
func (s *Scheduler) Add(ctx context.Context, rule Rule) (string, error) {
	if s == nil || s.core == nil {
		return "", errdefs.NotAvailablef("memory scheduler: nil scheduler")
	}
	if err := rule.Value.validate(); err != nil {
		return "", err
	}
	return s.core.Add(ctx, rule)
}

// Remove disarms and deletes a recurring rule.
func (s *Scheduler) Remove(ctx context.Context, ruleID string) (bool, error) {
	if s == nil || s.core == nil {
		return false, nil
	}
	return s.core.Remove(ctx, ruleID)
}

// Rules returns armed rule IDs in stable order.
func (s *Scheduler) Rules() []string {
	if s == nil || s.core == nil {
		return nil
	}
	return s.core.Rules()
}

// Restore re-arms persisted rules through the generic scheduler.
func (s *Scheduler) Restore(ctx context.Context) (int, error) {
	if s == nil || s.core == nil {
		return 0, errdefs.NotAvailablef("memory scheduler: nil scheduler")
	}
	return s.core.Restore(ctx)
}

// adapter synchronously maps Tasks to Runtime maintenance requests.
// All compact and archive executions share one gate so rules cannot mutate the
// same backing stores concurrently.
type adapter struct {
	runtime *sdkmemory.Runtime
	scope   sdkmemory.Scope
	clock   sdkmemory.Clock
	ctx     context.Context
	cancel  context.CancelFunc
	serial  chan struct{}
	once    sync.Once
}

// Dispatch executes one maintenance task using the scheduler lifecycle context.
func (d *adapter) Dispatch(_ context.Context, scheduleID string, task Task) (corescheduler.Outstanding, error) {
	if err := task.validate(); err != nil {
		return nil, err
	}
	select {
	case d.serial <- struct{}{}:
		defer func() { <-d.serial }()
	case <-d.ctx.Done():
		return nil, d.ctx.Err()
	}
	if err := d.ctx.Err(); err != nil {
		return nil, err
	}

	cutoff := d.clock.Now().Add(-task.OlderThan)
	switch task.Operation {
	case OperationCompact:
		_, err := d.runtime.ExecuteCompact(d.ctx, sdkmemory.CompactRequest{
			Scope:     d.scope,
			OlderThan: cutoff,
			Keep:      task.Keep,
		})
		if err != nil {
			return nil, fmt.Errorf("memory scheduler: compact schedule %q: %w", scheduleID, err)
		}
	case OperationArchive:
		_, err := d.runtime.ExecuteArchive(d.ctx, sdkmemory.ArchiveRequest{
			Scope:       d.scope,
			OlderThan:   cutoff,
			Destination: task.Destination,
		})
		if err != nil {
			return nil, fmt.Errorf("memory scheduler: archive schedule %q: %w", scheduleID, err)
		}
	}
	return completedOutstanding{}, nil
}

func (d *adapter) close() {
	d.once.Do(d.cancel)
}

type completedOutstanding struct{}

func (completedOutstanding) IsOutstanding(context.Context) (bool, error) {
	return false, nil
}

func (t Task) validate() error {
	if t.OlderThan <= 0 {
		return errdefs.Validationf("memory scheduler: task older_than must be greater than zero")
	}
	switch t.Operation {
	case OperationCompact:
		if t.Keep < 0 {
			return errdefs.Validationf("memory scheduler: compact keep must not be negative")
		}
		if t.Destination != "" {
			return errdefs.Validationf("memory scheduler: compact destination is not supported")
		}
	case OperationArchive:
		if strings.TrimSpace(t.Destination) == "" {
			return errdefs.Validationf("memory scheduler: archive destination is required")
		}
		if t.Keep != 0 {
			return errdefs.Validationf("memory scheduler: archive keep is not supported")
		}
	default:
		return errdefs.Validationf("memory scheduler: unsupported operation %q", t.Operation)
	}
	return nil
}

func lifecycleRules(lifecycle LifecycleSpec) []Rule {
	rules := make([]Rule, 0, 2)
	if lifecycle.Compact.Cron != "" {
		rules = append(rules, Rule{
			ID:      CompactRuleID,
			Cron:    lifecycle.Compact.Cron,
			Overlap: corescheduler.OverlapSkip,
			Value: Task{
				Operation: OperationCompact,
				OlderThan: lifecycle.Compact.OlderThan,
				Keep:      lifecycle.Compact.Keep,
			},
		})
	}
	if lifecycle.Archive.Cron != "" {
		rules = append(rules, Rule{
			ID:      ArchiveRuleID,
			Cron:    lifecycle.Archive.Cron,
			Overlap: corescheduler.OverlapSkip,
			Value: Task{
				Operation:   OperationArchive,
				OlderThan:   lifecycle.Archive.OlderThan,
				Destination: lifecycle.Archive.Destination,
			},
		})
	}
	return rules
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
