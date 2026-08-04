// Package scheduler adapts sdk/delegation requests to sdk/scheduler.
package scheduler

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"

	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
)

const (
	// PayloadKind identifies scheduled delegation request payloads.
	PayloadKind = "delegation.request"
	// PayloadVersion is the current scheduled delegation request schema.
	PayloadVersion = 1

	defaultDelegationPollInterval = 250 * time.Millisecond
)

type (
	// Task is a scheduled delegation request.
	Task = sdkdelegation.Request
	// Rule is a recurring scheduled delegation request.
	Rule = sdkscheduler.TypedRule[Task]
	// Scheduler is a typed scheduler registration for delegation tasks.
	Scheduler = sdkscheduler.Registration[Task]
)

// Option configures a Scheduler.
type Option func(*options) error

type options struct {
	workerOptions          []sdkscheduler.WorkerOption
	delegationPollInterval time.Duration
}

// WithWorkerOptions configures worker leasing, concurrency, and server polling.
func WithWorkerOptions(workerOptions ...sdkscheduler.WorkerOption) Option {
	return func(options *options) error {
		for _, option := range workerOptions {
			if option == nil {
				return errdefs.Validationf("delegation scheduler: worker option must not be nil")
			}
		}
		options.workerOptions = append(options.workerOptions, workerOptions...)
		return nil
	}
}

// WithDelegationPollInterval sets the delay between delegation status reads.
func WithDelegationPollInterval(interval time.Duration) Option {
	return func(options *options) error {
		if interval <= 0 {
			return errdefs.Validationf(
				"delegation scheduler: delegation poll interval must be greater than zero",
			)
		}
		options.delegationPollInterval = interval
		return nil
	}
}

// New constructs a namespace-scoped delegation scheduler. The Server and
// delegation Service remain caller-owned.
func New(
	ctx context.Context,
	server sdkscheduler.Server,
	namespace string,
	service sdkdelegation.Service,
	opts ...Option,
) (*Scheduler, error) {
	if isNil(service) {
		return nil, errdefs.Validationf("delegation scheduler: service is required")
	}

	settings := options{delegationPollInterval: defaultDelegationPollInterval}
	for _, option := range opts {
		if option == nil {
			return nil, errdefs.Validationf("delegation scheduler: option must not be nil")
		}
		if err := option(&settings); err != nil {
			return nil, err
		}
	}

	return sdkscheduler.Register(ctx, server, sdkscheduler.RegistrationSpec[Task]{
		Namespace:      namespace,
		PayloadKind:    PayloadKind,
		PayloadVersion: PayloadVersion,
		Handler: &handler{
			service:      service,
			pollInterval: settings.delegationPollInterval,
		},
		WorkerOptions: append([]sdkscheduler.WorkerOption(nil), settings.workerOptions...),
	})
}

type handler struct {
	service      sdkdelegation.Service
	pollInterval time.Duration
}

func (h *handler) Handle(
	ctx context.Context,
	delivery sdkscheduler.Delivery,
	request Task,
) error {
	request.IdempotencyKey = delivery.ID
	if err := request.Validate(); err != nil {
		return err
	}
	request.Metadata = maps.Clone(request.Metadata)
	if request.Metadata == nil {
		request.Metadata = make(map[string]string, 3)
	}
	request.Metadata[sdkscheduler.MetaScheduleID] = delivery.ScheduleID
	request.Metadata[sdkscheduler.MetaDeliveryID] = delivery.ID
	request.Metadata[sdkscheduler.MetaExecutionID] = delivery.ExecutionID

	response, err := h.service.Delegate(ctx, request)
	if err != nil {
		return fmt.Errorf(
			"delegation scheduler: delegate delivery %q: %w",
			delivery.ID,
			err,
		)
	}
	if err := validateResponse("Delegate", response); err != nil {
		return err
	}
	delegationID := response.ID
	for !response.Status.Terminal() {
		timer := time.NewTimer(h.pollInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		}
		response, err = h.service.Get(ctx, delegationID)
		if err != nil {
			return fmt.Errorf(
				"delegation scheduler: get delegation %q for delivery %q: %w",
				delegationID,
				delivery.ID,
				err,
			)
		}
		if err := validateResponse("Get", response); err != nil {
			return err
		}
		if response.ID != delegationID {
			return errdefs.Internalf(
				"delegation scheduler: Get response ID %q does not match delegation %q",
				response.ID,
				delegationID,
			)
		}
	}
	return terminalError(response)
}

func validateResponse(operation string, response sdkdelegation.Response) error {
	if err := response.Validate(); err != nil {
		return errdefs.Internal(fmt.Errorf(
			"delegation scheduler: invalid %s response: %w",
			operation,
			err,
		))
	}
	return nil
}

func terminalError(response sdkdelegation.Response) error {
	switch response.Status {
	case sdkdelegation.StatusSucceeded:
		return nil
	case sdkdelegation.StatusFailed:
		return fmt.Errorf(
			"delegation scheduler: delegation %q failed: %s",
			response.ID,
			response.Error,
		)
	case sdkdelegation.StatusCanceled:
		reason := response.Error
		if strings.TrimSpace(reason) == "" {
			reason = "canceled"
		}
		return fmt.Errorf(
			"delegation scheduler: delegation %q canceled: %s",
			response.ID,
			reason,
		)
	default:
		return errdefs.Internalf(
			"delegation scheduler: delegation %q reached invalid terminal status %q",
			response.ID,
			response.Status,
		)
	}
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

var _ sdkscheduler.Handler[Task] = (*handler)(nil)
