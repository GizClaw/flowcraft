package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/rs/xid"
)

// TypedRule is the typed client view of a recurring rule.
type TypedRule[T any] struct {
	Namespace string  `json:"namespace"`
	ID        string  `json:"id"`
	Cron      string  `json:"cron"`
	Timezone  string  `json:"timezone"`
	Overlap   Overlap `json:"overlap,omitempty"`
	Task      T       `json:"task"`
}

// ClientOption configures a typed client.
type ClientOption func(*clientOptions) error

type clientOptions struct {
	now func() time.Time
}

// WithClientClock overrides the clock used by After.
func WithClientClock(now func() time.Time) ClientOption {
	return func(options *clientOptions) error {
		if now == nil {
			return invalidf("client clock must not be nil")
		}
		options.now = now
		return nil
	}
}

// Client adds a typed task surface to a namespace-scoped Control.
type Client[T any] struct {
	control   Control
	namespace string
	kind      string
	version   int
	now       func() time.Time
}

// NewClient constructs a namespace- and payload-version-scoped client.
func NewClient[T any](
	control Control,
	namespace, payloadKind string,
	payloadVersion int,
	options ...ClientOption,
) (*Client[T], error) {
	if isNilInterface(control) {
		return nil, invalidf("client Control is required")
	}
	if err := required("client namespace", namespace); err != nil {
		return nil, err
	}
	if err := required("client payload kind", payloadKind); err != nil {
		return nil, err
	}
	if payloadVersion <= 0 {
		return nil, invalidf("client payload version must be greater than zero")
	}
	config := clientOptions{now: time.Now}
	for _, option := range options {
		if option == nil {
			return nil, invalidf("client option must not be nil")
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	return &Client[T]{
		control:   control,
		namespace: namespace,
		kind:      payloadKind,
		version:   payloadVersion,
		now:       config.now,
	}, nil
}

// PutRule creates or replaces a caller-identified recurring rule. If Control
// fails after an automatic ID is generated, the returned partial rule includes
// that ID and the client namespace so callers can safely retry it.
func (c *Client[T]) PutRule(ctx context.Context, input TypedRule[T]) (TypedRule[T], error) {
	if input.Namespace != "" && input.Namespace != c.namespace {
		return TypedRule[T]{}, invalidf(
			"typed rule namespace %q does not match client namespace %q",
			input.Namespace, c.namespace,
		)
	}
	if input.ID == "" {
		input.ID = xid.New().String()
	}
	input.Namespace = c.namespace
	if err := validateTaskValue(input.Task); err != nil {
		return TypedRule[T]{}, err
	}
	payload, err := NewJSONPayload(c.kind, c.version, input.Task)
	if err != nil {
		return TypedRule[T]{}, err
	}
	wire := Rule{
		Namespace: c.namespace,
		ID:        input.ID,
		Cron:      input.Cron,
		Timezone:  input.Timezone,
		Overlap:   input.Overlap,
		Task:      Task{Payload: payload},
	}
	if err := wire.Validate(); err != nil {
		return TypedRule[T]{}, err
	}
	if err := c.control.PutRule(ctx, wire); err != nil {
		return input, err
	}
	return input, nil
}

// At schedules task for an absolute instant with an automatically generated ID.
// On a Control error, it returns the fully populated Once alongside the error
// so callers can retry with AtID after an ambiguous or lost response.
func (c *Client[T]) At(ctx context.Context, at time.Time, task T) (Once, error) {
	return c.AtID(ctx, "", at, task)
}

// AtID schedules task for an absolute instant using id. An empty ID is
// generated before the Control call so one in-flight request always has a
// stable idempotency key. Control errors return the populated Once and error.
func (c *Client[T]) AtID(ctx context.Context, id string, at time.Time, task T) (Once, error) {
	if err := validateTaskValue(task); err != nil {
		return Once{}, err
	}
	if id == "" {
		id = xid.New().String()
	}
	payload, err := NewJSONPayload(c.kind, c.version, task)
	if err != nil {
		return Once{}, err
	}
	once := Once{
		Namespace: c.namespace,
		ID:        id,
		At:        at,
		Task:      Task{Payload: payload},
	}
	if err := once.Validate(); err != nil {
		return Once{}, err
	}
	if err := c.control.ScheduleOnce(ctx, once); err != nil {
		return once, err
	}
	return once, nil
}

type taskValidator interface {
	Validate() error
}

func validateTaskValue[T any](task T) error {
	validator, ok := any(task).(taskValidator)
	if !ok {
		return nil
	}
	if isNilInterface(validator) {
		return invalidf("task validator must not be nil")
	}
	if err := validator.Validate(); err != nil {
		return errdefs.Validation(fmt.Errorf("scheduler: validate task: %w", err))
	}
	return nil
}

// After schedules task at the absolute instant computed when this method starts.
func (c *Client[T]) After(ctx context.Context, delay time.Duration, task T) (Once, error) {
	return c.AfterID(ctx, "", delay, task)
}

// AfterID is After with a caller-provided idempotency key.
func (c *Client[T]) AfterID(ctx context.Context, id string, delay time.Duration, task T) (Once, error) {
	if delay < 0 {
		return Once{}, invalidf("delay must not be negative")
	}
	at := c.now().Add(delay)
	return c.AtID(ctx, id, at, task)
}

// Cancel cancels a one-shot schedule in this client's namespace.
func (c *Client[T]) Cancel(ctx context.Context, id string) error {
	if err := required("schedule ID", id); err != nil {
		return err
	}
	return c.control.CancelOnce(ctx, c.namespace, id)
}

// Remove deletes a recurring rule in this client's namespace.
func (c *Client[T]) Remove(ctx context.Context, id string) error {
	if err := required("rule ID", id); err != nil {
		return err
	}
	return c.control.DeleteRule(ctx, c.namespace, id)
}

// List returns this namespace's recurring rules with strictly decoded tasks.
func (c *Client[T]) List(ctx context.Context) ([]TypedRule[T], error) {
	rules, err := c.control.ListRules(ctx, c.namespace)
	if err != nil {
		return nil, err
	}
	typed := make([]TypedRule[T], 0, len(rules))
	for _, rule := range rules {
		if err := rule.Validate(); err != nil {
			return nil, err
		}
		if rule.Namespace != c.namespace {
			return nil, invalidf(
				"server returned namespace %q for client namespace %q",
				rule.Namespace, c.namespace,
			)
		}
		task, err := DecodeJSON[T](rule.Task.Payload, c.kind, c.version)
		if err != nil {
			return nil, err
		}
		typed = append(typed, TypedRule[T]{
			Namespace: rule.Namespace,
			ID:        rule.ID,
			Cron:      rule.Cron,
			Timezone:  rule.Timezone,
			Overlap:   rule.Overlap,
			Task:      task,
		})
	}
	return typed, nil
}
