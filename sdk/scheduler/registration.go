package scheduler

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const registrationRollbackTimeout = 5 * time.Second

// RegistrationSpec describes one typed namespace served through leased work.
// Registering does not mount a callback on Server: Handler is invoked only by
// the Worker polling Server's WorkSource.
type RegistrationSpec[T any] struct {
	Namespace      string
	PayloadKind    string
	PayloadVersion int
	Rules          []TypedRule[T]
	Handler        Handler[T]
	ClientOptions  []ClientOption
	WorkerOptions  []WorkerOption
}

type installedRule[T any] struct {
	id       string
	previous TypedRule[T]
	replaced bool
}

// Registration combines one typed Client with one background Worker. Server is
// borrowed; Close stops only the Worker.
type Registration[T any] struct {
	client *Client[T]
	worker *Worker[T]

	mu        sync.Mutex
	started   bool
	closed    bool
	cancel    context.CancelFunc
	done      chan struct{}
	runErr    error
	installed []installedRule[T]

	rollbackOnce sync.Once
	rollbackErr  error
}

// Register validates spec, builds its Client and Worker, and installs rules in
// order. If installation fails, successfully installed rules are rolled back in
// reverse order: replaced rules are restored and newly created rules are
// removed. All original and rollback failures are joined.
func Register[T any](
	ctx context.Context,
	server Server,
	spec RegistrationSpec[T],
) (*Registration[T], error) {
	if ctx == nil {
		return nil, invalidf("registration context must not be nil")
	}
	if isNilInterface(server) {
		return nil, invalidf("registration Server is required")
	}
	if err := required("RegistrationSpec.Namespace", spec.Namespace); err != nil {
		return nil, err
	}
	if err := required("RegistrationSpec.PayloadKind", spec.PayloadKind); err != nil {
		return nil, err
	}
	if spec.PayloadVersion <= 0 {
		return nil, invalidf("RegistrationSpec.PayloadVersion must be greater than zero")
	}
	if isNilInterface(spec.Handler) {
		return nil, invalidf("RegistrationSpec.Handler is required")
	}

	client, err := NewClient[T](
		server,
		spec.Namespace,
		spec.PayloadKind,
		spec.PayloadVersion,
		spec.ClientOptions...,
	)
	if err != nil {
		return nil, err
	}
	worker, err := NewWorker[T](
		server,
		spec.Namespace,
		spec.PayloadKind,
		spec.PayloadVersion,
		spec.Handler,
		spec.WorkerOptions...,
	)
	if err != nil {
		return nil, err
	}

	existing, err := client.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list existing rules before registration: %w", err)
	}
	currentByID := make(map[string]TypedRule[T], len(existing))
	for _, rule := range existing {
		currentByID[rule.ID] = rule
	}
	installed := make([]installedRule[T], 0, len(spec.Rules))
	for index, rule := range spec.Rules {
		created, putErr := client.PutRule(ctx, rule)
		if putErr == nil {
			previous, replaced := currentByID[created.ID]
			installed = append(installed, installedRule[T]{
				id:       created.ID,
				previous: previous,
				replaced: replaced,
			})
			currentByID[created.ID] = created
			continue
		}
		failures := []error{fmt.Errorf("scheduler: register rule %d: %w", index, putErr)}
		// PutRule returns the resolved identity when the Control call fails.
		// The server may already have applied that ambiguous request, so roll
		// the current item back before reversing earlier successful installs.
		if created.ID != "" {
			previous, replaced := currentByID[created.ID]
			installed = append(installed, installedRule[T]{
				id:       created.ID,
				previous: previous,
				replaced: replaced,
			})
		}
		failures = append(failures, rollbackInstalled(ctx, client, installed)...)
		return nil, errors.Join(failures...)
	}

	return &Registration[T]{
		client:    client,
		worker:    worker,
		installed: installed,
	}, nil
}

func restoreRule[T any](ctx context.Context, client *Client[T], rule TypedRule[T]) error {
	_, err := client.PutRule(ctx, rule)
	return err
}

func rollbackInstalled[T any](
	ctx context.Context,
	client *Client[T],
	installed []installedRule[T],
) []error {
	return rollbackInstalledWithTimeout(ctx, client, installed, registrationRollbackTimeout)
}

func rollbackInstalledWithTimeout[T any](
	ctx context.Context,
	client *Client[T],
	installed []installedRule[T],
	timeout time.Duration,
) []error {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	var failures []error
	for index := len(installed) - 1; index >= 0; index-- {
		item := installed[index]
		rollbackCtx, cancel := context.WithTimeout(base, timeout)
		if item.replaced {
			err := restoreRule(rollbackCtx, client, item.previous)
			cancel()
			if err != nil {
				failures = append(failures, fmt.Errorf(
					"scheduler: restore rule %q during rollback: %w",
					item.id,
					err,
				))
			}
			continue
		}
		err := client.Remove(rollbackCtx, item.id)
		cancel()
		if err != nil && !errdefs.IsNotFound(err) {
			failures = append(failures, fmt.Errorf(
				"scheduler: roll back rule %q: %w",
				item.id,
				err,
			))
		}
	}
	return failures
}

// Rollback idempotently stops the Worker and reverses only the rules installed
// by Register. Rules added later through PutRule are not affected. Concurrent
// callers wait for and receive the same aggregate result.
func (r *Registration[T]) Rollback(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.rollbackOnce.Do(func() {
		failures := []error{r.Close()}
		failures = append(failures, rollbackInstalled(ctx, r.client, r.installed)...)
		r.rollbackErr = errors.Join(failures...)
	})
	return r.rollbackErr
}

// Start starts the background Worker exactly once. Repeated calls while it is
// running succeed. If it exited unexpectedly, Start returns its terminal error.
func (r *Registration[T]) Start() error {
	if r == nil {
		return invalidf("Registration is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errdefs.NotAvailablef("scheduler: registration closed")
	}
	if r.started {
		select {
		case <-r.done:
			if r.runErr != nil {
				return r.runErr
			}
			return errdefs.NotAvailablef("scheduler: registration worker stopped")
		default:
			return nil
		}
	}

	runCtx, cancel := context.WithCancel(context.Background())
	r.started = true
	r.cancel = cancel
	r.done = make(chan struct{})
	go r.run(runCtx)
	return nil
}

func (r *Registration[T]) run(ctx context.Context) {
	err := r.worker.Run(ctx)
	r.mu.Lock()
	r.runErr = err
	done := r.done
	r.mu.Unlock()
	close(done)
}

// Close idempotently stops and waits for the background Worker. Concurrent
// callers observe the same worker result. It never closes the borrowed Server.
func (r *Registration[T]) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.closed = true
	started, cancel, done := r.started, r.cancel, r.done
	r.mu.Unlock()

	if started {
		cancel()
		<-done
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runErr
}

// PutRule forwards to the typed Client.
func (r *Registration[T]) PutRule(ctx context.Context, rule TypedRule[T]) (TypedRule[T], error) {
	return r.client.PutRule(ctx, rule)
}

// After forwards to the typed Client.
func (r *Registration[T]) After(ctx context.Context, delay time.Duration, task T) (Once, error) {
	return r.client.After(ctx, delay, task)
}

// AfterID forwards to the typed Client.
func (r *Registration[T]) AfterID(ctx context.Context, id string, delay time.Duration, task T) (Once, error) {
	return r.client.AfterID(ctx, id, delay, task)
}

// At forwards to the typed Client.
func (r *Registration[T]) At(ctx context.Context, at time.Time, task T) (Once, error) {
	return r.client.At(ctx, at, task)
}

// AtID forwards to the typed Client.
func (r *Registration[T]) AtID(ctx context.Context, id string, at time.Time, task T) (Once, error) {
	return r.client.AtID(ctx, id, at, task)
}

// Cancel forwards to the typed Client.
func (r *Registration[T]) Cancel(ctx context.Context, id string) error {
	return r.client.Cancel(ctx, id)
}

// Remove forwards to the typed Client.
func (r *Registration[T]) Remove(ctx context.Context, id string) error {
	return r.client.Remove(ctx, id)
}

// List forwards to the typed Client.
func (r *Registration[T]) List(ctx context.Context) ([]TypedRule[T], error) {
	return r.client.List(ctx)
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflection := reflect.ValueOf(value)
	switch reflection.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflection.IsNil()
	default:
		return false
	}
}
