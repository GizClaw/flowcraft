// Package memory adapts memory maintenance operations to sdk/scheduler.
package memory

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	"github.com/GizClaw/flowcraft/sdkx/memory/config"
)

const (
	// PayloadKind identifies memory maintenance task payloads.
	PayloadKind = "memory.maintenance"
	// PayloadVersion is the current memory maintenance task schema.
	PayloadVersion = 1

	// CompactRuleID is the stable ID of the lifecycle compact rule.
	CompactRuleID = "memory-compact"
	// ArchiveRuleID is the stable ID of the lifecycle archive rule.
	ArchiveRuleID = "memory-archive"
)

// CompactTask configures one compaction execution.
type CompactTask struct {
	OlderThan time.Duration `json:"older_than"`
	Keep      int           `json:"keep,omitempty"`
}

// ArchiveTask configures one archive execution.
type ArchiveTask struct {
	OlderThan   time.Duration `json:"older_than"`
	Destination string        `json:"destination"`
}

// Task is a typed memory maintenance payload. Exactly one operation is set.
type Task struct {
	Compact *CompactTask `json:"compact,omitempty"`
	Archive *ArchiveTask `json:"archive,omitempty"`
}

// Validate checks that exactly one complete maintenance operation is present.
func (t Task) Validate() error {
	if (t.Compact == nil) == (t.Archive == nil) {
		return errdefs.Validationf("memory scheduler: task must contain exactly one of compact or archive")
	}
	if t.Compact != nil {
		if t.Compact.OlderThan <= 0 {
			return errdefs.Validationf("memory scheduler: compact older_than must be greater than zero")
		}
		if t.Compact.Keep < 0 {
			return errdefs.Validationf("memory scheduler: compact keep must not be negative")
		}
		return nil
	}
	if t.Archive.OlderThan <= 0 {
		return errdefs.Validationf("memory scheduler: archive older_than must be greater than zero")
	}
	if strings.TrimSpace(t.Archive.Destination) == "" {
		return errdefs.Validationf("memory scheduler: archive destination is required")
	}
	return nil
}

type (
	// LifecycleSpec is the deploy configuration accepted by New.
	LifecycleSpec = config.LifecycleSpec
	// Rule is a recurring memory maintenance task.
	Rule = sdkscheduler.TypedRule[Task]
	// Scheduler registers and executes memory maintenance tasks.
	Scheduler = sdkscheduler.Registration[Task]
)

// Option configures a Scheduler.
type Option func(*options) error

type options struct {
	clock         sdkmemory.Clock
	workerOptions []sdkscheduler.WorkerOption
}

// WithClock overrides the runtime clock used to calculate retention cutoffs.
func WithClock(clock sdkmemory.Clock) Option {
	return func(options *options) error {
		if isNil(clock) {
			return errdefs.Validationf("memory scheduler: clock must not be nil")
		}
		options.clock = clock
		return nil
	}
}

// WithWorkerOptions configures worker lease and polling behavior.
func WithWorkerOptions(workerOptions ...sdkscheduler.WorkerOption) Option {
	return func(options *options) error {
		for _, option := range workerOptions {
			if option == nil {
				return errdefs.Validationf("memory scheduler: worker option must not be nil")
			}
		}
		options.workerOptions = append(options.workerOptions, workerOptions...)
		return nil
	}
}

// New constructs a scheduler and registers each enabled lifecycle rule with ctx.
// Empty lifecycle blocks are disabled. The Server and Runtime remain caller-owned.
func New(
	ctx context.Context,
	server sdkscheduler.Server,
	namespace string,
	rt *sdkmemory.Runtime,
	lifecycle LifecycleSpec,
	opts ...Option,
) (*Scheduler, error) {
	if ctx == nil {
		return nil, errdefs.Validationf("memory scheduler: context is required")
	}
	if isNil(server) {
		return nil, errdefs.Validationf("memory scheduler: server is required")
	}
	if strings.TrimSpace(namespace) == "" {
		return nil, errdefs.Validationf("memory scheduler: namespace is required")
	}
	if rt == nil {
		return nil, errdefs.Validationf("memory scheduler: runtime is required")
	}
	if err := lifecycle.Validate(); err != nil {
		return nil, errdefs.Validation(err)
	}

	settings := options{clock: rt.Spec().Clock}
	for _, option := range opts {
		if option == nil {
			return nil, errdefs.Validationf("memory scheduler: option must not be nil")
		}
		if err := option(&settings); err != nil {
			return nil, err
		}
	}
	if isNil(settings.clock) {
		return nil, errdefs.Validationf("memory scheduler: clock must not be nil")
	}

	scope := rt.Spec().DefaultScope
	if scope.IsZero() {
		scope = sdkmemory.Scope{RuntimeID: rt.Spec().RuntimeID}
	}
	handler := &maintenanceHandler{
		runtime: rt,
		scope:   scope,
		clock:   settings.clock,
	}
	workerOptions := append([]sdkscheduler.WorkerOption(nil), settings.workerOptions...)
	workerOptions = append(workerOptions, sdkscheduler.WithMaxConcurrency(1))
	registration, err := sdkscheduler.Register(ctx, server, sdkscheduler.RegistrationSpec[Task]{
		Namespace:      namespace,
		PayloadKind:    PayloadKind,
		PayloadVersion: PayloadVersion,
		Rules:          lifecycleRules(namespace, lifecycle),
		Handler:        handler,
		ClientOptions: []sdkscheduler.ClientOption{
			sdkscheduler.WithClientClock(settings.clock.Now),
		},
		WorkerOptions: workerOptions,
	})
	if err != nil {
		return nil, err
	}
	return registration, nil
}

type maintenanceHandler struct {
	runtime *sdkmemory.Runtime
	scope   sdkmemory.Scope
	clock   sdkmemory.Clock
}

func (h *maintenanceHandler) Handle(
	ctx context.Context,
	delivery sdkscheduler.Delivery,
	task Task,
) error {
	if err := task.Validate(); err != nil {
		return err
	}
	if task.Compact != nil {
		_, err := h.runtime.ExecuteCompact(ctx, sdkmemory.CompactRequest{
			Scope:     h.scope,
			OlderThan: h.clock.Now().Add(-task.Compact.OlderThan),
			Keep:      task.Compact.Keep,
		})
		if err != nil {
			return fmt.Errorf("memory scheduler: compact schedule %q: %w", delivery.ScheduleID, err)
		}
		return nil
	}
	_, err := h.runtime.ExecuteArchive(ctx, sdkmemory.ArchiveRequest{
		Scope:       h.scope,
		OlderThan:   h.clock.Now().Add(-task.Archive.OlderThan),
		Destination: task.Archive.Destination,
	})
	if err != nil {
		return fmt.Errorf("memory scheduler: archive schedule %q: %w", delivery.ScheduleID, err)
	}
	return nil
}

func lifecycleRules(namespace string, lifecycle LifecycleSpec) []Rule {
	rules := make([]Rule, 0, 2)
	if lifecycle.Compact.Cron != "" {
		rules = append(rules, Rule{
			Namespace: namespace,
			ID:        CompactRuleID,
			Cron:      lifecycle.Compact.Cron,
			Timezone:  "UTC",
			Overlap:   sdkscheduler.OverlapSkip,
			Task: Task{Compact: &CompactTask{
				OlderThan: lifecycle.Compact.OlderThan,
				Keep:      lifecycle.Compact.Keep,
			}},
		})
	}
	if lifecycle.Archive.Cron != "" {
		rules = append(rules, Rule{
			Namespace: namespace,
			ID:        ArchiveRuleID,
			Cron:      lifecycle.Archive.Cron,
			Timezone:  "UTC",
			Overlap:   sdkscheduler.OverlapSkip,
			Task: Task{Archive: &ArchiveTask{
				OlderThan:   lifecycle.Archive.OlderThan,
				Destination: lifecycle.Archive.Destination,
			}},
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

var _ sdkscheduler.Handler[Task] = (*maintenanceHandler)(nil)
