// Package scheduler provides an optional sdk/delegation dispatcher adapter
// for the generic sdkx/scheduler package.
package scheduler

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"time"

	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	corescheduler "github.com/GizClaw/flowcraft/sdkx/scheduler"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	// MetaScheduleID correlates delegation requests with their schedule.
	MetaScheduleID = corescheduler.MetaScheduleID
	// AttrDelegationID identifies a dispatched delegation in telemetry.
	AttrDelegationID = "scheduler.delegation.id"
)

type (
	// Rule is a recurring delegation request.
	Rule = corescheduler.Rule[sdkdelegation.Request]
	// RuleStore persists delegation scheduling rules.
	RuleStore = corescheduler.RuleStore[sdkdelegation.Request]
	// Option configures a delegation Scheduler.
	Option = corescheduler.Option[sdkdelegation.Request]
)

// WithRuleStore enables recurring-rule persistence.
func WithRuleStore(store RuleStore) Option {
	return corescheduler.WithRuleStore(store)
}

// Scheduler schedules asynchronous delegation requests.
type Scheduler struct {
	core *corescheduler.Scheduler[sdkdelegation.Request]
}

// New creates a delegation scheduler backed by service.
func New(service sdkdelegation.Service, opts ...Option) (*Scheduler, error) {
	if isNil(service) {
		return nil, errdefs.Validationf("delegation scheduler: service is required")
	}
	adapter := &adapter{service: service}
	core, err := corescheduler.New(adapter, opts...)
	if err != nil {
		return nil, err
	}
	return &Scheduler{core: core}, nil
}

// Start begins cron evaluation.
func (s *Scheduler) Start() {
	if s != nil && s.core != nil {
		s.core.Start()
	}
}

// Close stops cron evaluation and pending delays.
func (s *Scheduler) Close() error {
	if s == nil {
		return nil
	}
	return s.core.Close()
}

// After schedules one asynchronous delegation.
func (s *Scheduler) After(ctx context.Context, delay time.Duration, request sdkdelegation.Request) (string, error) {
	request, err := normalizeRequest(request, "")
	if err != nil {
		return "", err
	}
	return s.core.After(ctx, delay, request)
}

// CancelDelay cancels a pending delay.
func (s *Scheduler) CancelDelay(handle string) bool {
	return s != nil && s.core.CancelDelay(handle)
}

// Add validates and registers a recurring asynchronous delegation.
func (s *Scheduler) Add(ctx context.Context, rule Rule) (string, error) {
	request, err := normalizeRequest(rule.Value, "")
	if err != nil {
		return "", err
	}
	rule.Value = request
	return s.core.Add(ctx, rule)
}

// Remove disarms and deletes a recurring rule.
// The bool reports whether an armed rule existed; persistence failures are
// returned after the in-memory rule has been disarmed.
func (s *Scheduler) Remove(ctx context.Context, ruleID string) (bool, error) {
	if s == nil {
		return false, nil
	}
	return s.core.Remove(ctx, ruleID)
}

// Rules returns armed rule IDs.
func (s *Scheduler) Rules() []string {
	if s == nil {
		return nil
	}
	return s.core.Rules()
}

// Restore re-arms persisted rules through the generic scheduler.
func (s *Scheduler) Restore(ctx context.Context) (int, error) {
	if s == nil {
		return 0, errdefs.NotAvailablef("delegation scheduler: nil scheduler")
	}
	return s.core.Restore(ctx)
}

type adapter struct {
	service sdkdelegation.Service
}

func (a *adapter) Dispatch(ctx context.Context, scheduleID string, request sdkdelegation.Request) (corescheduler.Outstanding, error) {
	request, err := normalizeRequest(request, scheduleID)
	if err != nil {
		return nil, err
	}
	response, err := a.service.Delegate(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("delegation scheduler: delegate schedule %q: %w", scheduleID, err)
	}
	if err := response.Validate(); err != nil {
		return nil, errdefs.Internal(fmt.Errorf(
			"delegation scheduler: invalid Delegate response: %w", err))
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String(AttrDelegationID, response.ID))
	return &delegationOutstanding{
		service: a.service,
		id:      response.ID,
		status:  response.Status,
	}, nil
}

type delegationOutstanding struct {
	service sdkdelegation.Service
	id      string
	status  sdkdelegation.Status
}

func (o *delegationOutstanding) IsOutstanding(ctx context.Context) (bool, error) {
	if o.status.Terminal() {
		return false, nil
	}
	response, err := o.service.Get(ctx, o.id)
	if err != nil {
		return false, fmt.Errorf("delegation scheduler: get delegation %q: %w", o.id, err)
	}
	if err := response.Validate(); err != nil {
		return false, errdefs.Internal(fmt.Errorf(
			"delegation scheduler: invalid Get response for %q: %w", o.id, err))
	}
	o.status = response.Status
	return !response.Status.Terminal(), nil
}

func normalizeRequest(request sdkdelegation.Request, scheduleID string) (sdkdelegation.Request, error) {
	request.Mode = sdkdelegation.ModeAsync
	request.Metadata = cloneMetadata(request.Metadata)
	if scheduleID != "" {
		if request.Metadata == nil {
			request.Metadata = make(map[string]string, 1)
		}
		request.Metadata[MetaScheduleID] = scheduleID
	}
	if err := request.Validate(); err != nil {
		return sdkdelegation.Request{}, err
	}
	return request, nil
}

func cloneMetadata(metadata map[string]string) map[string]string {
	return maps.Clone(metadata)
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
