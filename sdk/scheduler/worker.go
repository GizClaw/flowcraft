package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	defaultLeaseDuration   = 30 * time.Second
	defaultRenewInterval   = 10 * time.Second
	defaultPollInterval    = 100 * time.Millisecond
	defaultShutdownTimeout = 5 * time.Second
	defaultRetryInitial    = 100 * time.Millisecond
	defaultRetryMaximum    = 5 * time.Second
	defaultMaxConcurrency  = 4
)

// Handler executes a typed delivery to a business terminal outcome.
//
// Handle must observe ctx and return promptly after cancellation. Go cannot
// safely force-stop a handler that violates this contract, so Worker.Run and
// Registration.Close intentionally remain blocked until every active handler
// has really exited.
type Handler[T any] interface {
	Handle(context.Context, Delivery, T) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc[T any] func(context.Context, Delivery, T) error

// Handle calls f.
func (f HandlerFunc[T]) Handle(ctx context.Context, delivery Delivery, task T) error {
	return f(ctx, delivery, task)
}

// WorkerOption configures a Worker.
type WorkerOption func(*workerOptions) error

type workerOptions struct {
	leaseDuration   time.Duration
	renewInterval   time.Duration
	pollInterval    time.Duration
	shutdownTimeout time.Duration
	retryInitial    time.Duration
	retryMaximum    time.Duration
	maxConcurrency  int
}

// WithLeaseDuration sets the lease requested by Claim and Renew.
func WithLeaseDuration(duration time.Duration) WorkerOption {
	return func(options *workerOptions) error {
		if duration <= 0 {
			return invalidf("worker lease duration must be greater than zero")
		}
		options.leaseDuration = duration
		return nil
	}
}

// WithRenewInterval sets how often active leases are renewed.
func WithRenewInterval(interval time.Duration) WorkerOption {
	return func(options *workerOptions) error {
		if interval <= 0 {
			return invalidf("worker renew interval must be greater than zero")
		}
		options.renewInterval = interval
		return nil
	}
}

// WithPollInterval sets the delay after Claim reports no available work.
func WithPollInterval(interval time.Duration) WorkerOption {
	return func(options *workerOptions) error {
		if interval <= 0 {
			return invalidf("worker poll interval must be greater than zero")
		}
		options.pollInterval = interval
		return nil
	}
}

// WithShutdownTimeout bounds terminal Complete retries and is advertised to
// the server as CompleteRequest.RetainUntil.
func WithShutdownTimeout(timeout time.Duration) WorkerOption {
	return func(options *workerOptions) error {
		if timeout <= 0 {
			return invalidf("worker shutdown timeout must be greater than zero")
		}
		options.shutdownTimeout = timeout
		return nil
	}
}

// WithRetryBackoff bounds exponential backoff for transient Claim, Renew, and
// Complete failures.
func WithRetryBackoff(initial, maximum time.Duration) WorkerOption {
	return func(options *workerOptions) error {
		if initial <= 0 {
			return invalidf("worker retry initial backoff must be greater than zero")
		}
		if maximum < initial {
			return invalidf("worker retry maximum backoff must not be shorter than initial backoff")
		}
		options.retryInitial = initial
		options.retryMaximum = maximum
		return nil
	}
}

// WithMaxConcurrency limits the number of claimed deliveries in flight.
func WithMaxConcurrency(maximum int) WorkerOption {
	return func(options *workerOptions) error {
		if maximum <= 0 {
			return invalidf("worker maximum concurrency must be greater than zero")
		}
		options.maxConcurrency = maximum
		return nil
	}
}

// Worker claims and executes typed tasks from one namespace.
type Worker[T any] struct {
	source          WorkSource
	handler         Handler[T]
	namespace       string
	kind            string
	version         int
	leaseDuration   time.Duration
	renewInterval   time.Duration
	pollInterval    time.Duration
	shutdownTimeout time.Duration
	retryInitial    time.Duration
	retryMaximum    time.Duration
	maxConcurrency  int
}

// NewWorker constructs a typed leased-work consumer.
func NewWorker[T any](
	source WorkSource,
	namespace, payloadKind string,
	payloadVersion int,
	handler Handler[T],
	options ...WorkerOption,
) (*Worker[T], error) {
	if isNilInterface(source) {
		return nil, invalidf("worker WorkSource is required")
	}
	if isNilInterface(handler) {
		return nil, invalidf("worker Handler is required")
	}
	if err := required("worker namespace", namespace); err != nil {
		return nil, err
	}
	if err := required("worker payload kind", payloadKind); err != nil {
		return nil, err
	}
	if payloadVersion <= 0 {
		return nil, invalidf("worker payload version must be greater than zero")
	}
	config := workerOptions{
		leaseDuration:   defaultLeaseDuration,
		renewInterval:   defaultRenewInterval,
		pollInterval:    defaultPollInterval,
		shutdownTimeout: defaultShutdownTimeout,
		retryInitial:    defaultRetryInitial,
		retryMaximum:    defaultRetryMaximum,
		maxConcurrency:  defaultMaxConcurrency,
	}
	for _, option := range options {
		if option == nil {
			return nil, invalidf("worker option must not be nil")
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	if config.renewInterval >= config.leaseDuration {
		return nil, invalidf("worker renew interval must be shorter than lease duration")
	}
	return &Worker[T]{
		source:          source,
		handler:         handler,
		namespace:       namespace,
		kind:            payloadKind,
		version:         payloadVersion,
		leaseDuration:   config.leaseDuration,
		renewInterval:   config.renewInterval,
		pollInterval:    config.pollInterval,
		shutdownTimeout: config.shutdownTimeout,
		retryInitial:    config.retryInitial,
		retryMaximum:    config.retryMaximum,
		maxConcurrency:  config.maxConcurrency,
	}, nil
}

// Run claims work until ctx is canceled or a permanent server/protocol error
// occurs. Transient Claim and Renew failures use bounded exponential backoff;
// lease loss cancels only the affected delivery. Run cancels active handlers,
// waits for them, and attempts canceled completion before returning. No
// separate Close or Wait call is required.
func (w *Worker[T]) Run(ctx context.Context) error {
	if ctx == nil {
		return invalidf("worker Run context must not be nil")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		workWG   sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	report := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errMu.Unlock()
	}
	currentError := func() error {
		errMu.Lock()
		defer errMu.Unlock()
		return firstErr
	}
	result := func() error {
		workWG.Wait()
		return currentError()
	}

	slots := make(chan struct{}, w.maxConcurrency)
	claimBackoff := w.retryInitial
	for {
		select {
		case slots <- struct{}{}:
		case <-runCtx.Done():
			return result()
		}

		delivery, err := w.source.Claim(runCtx, ClaimRequest{
			Namespace:     w.namespace,
			LeaseDuration: w.leaseDuration,
		})
		if err != nil {
			<-slots
			if runCtx.Err() != nil && errors.Is(err, runCtx.Err()) {
				return result()
			}
			if permanentWorkerError(err) {
				report(err)
				return result()
			}
			if !waitFor(runCtx, claimBackoff) {
				return result()
			}
			claimBackoff = nextBackoff(claimBackoff, w.retryMaximum)
			continue
		}
		claimBackoff = w.retryInitial
		if delivery == nil {
			<-slots
			if !waitFor(runCtx, w.pollInterval) {
				return result()
			}
			continue
		}

		workWG.Add(1)
		go func(delivery Delivery) {
			defer workWG.Done()
			defer func() { <-slots }()
			w.handle(runCtx, delivery, report)
		}(*delivery)
	}
}

func (w *Worker[T]) handle(runCtx context.Context, delivery Delivery, report func(error)) {
	workCtx, cancelWork := context.WithCancel(runCtx)
	defer cancelWork()

	stopRenew := make(chan struct{})
	leaseLost := make(chan bool, 1)
	var renewWG sync.WaitGroup
	renewWG.Add(1)
	go func() {
		defer renewWG.Done()
		delay := w.renewInterval
		backoff := w.retryInitial
		leaseUntil := delivery.LeaseUntil
		for {
			if !waitForAny(workCtx, stopRenew, delay) {
				return
			}
			startedAt := time.Now()
			err := w.source.Renew(workCtx, RenewRequest{
				ExecutionID:   delivery.ExecutionID,
				LeaseToken:    delivery.LeaseToken,
				LeaseDuration: w.leaseDuration,
			})
			if err == nil {
				leaseUntil = startedAt.Add(w.leaseDuration)
				delay = w.renewInterval
				backoff = w.retryInitial
				continue
			}
			if workCtx.Err() != nil {
				return
			}
			if errdefs.IsConflict(err) || errdefs.IsNotFound(err) {
				leaseLost <- true
				cancelWork()
				return
			}
			if permanentWorkerError(err) {
				leaseLost <- false
				report(err)
				cancelWork()
				return
			}
			now := time.Now()
			if !now.Before(leaseUntil) || !now.Add(backoff).Before(leaseUntil) {
				leaseLost <- true
				cancelWork()
				return
			}
			delay = backoff
			backoff = nextBackoff(backoff, w.retryMaximum)
		}
	}()

	status := StatusSucceeded
	errorText := ""
	var task T
	if validationErr := delivery.Validate(); validationErr != nil {
		status = StatusFailed
		errorText = validationErr.Error()
	} else if delivery.Namespace != w.namespace {
		status = StatusFailed
		errorText = invalidf(
			"delivery namespace %q does not match worker namespace %q",
			delivery.Namespace, w.namespace,
		).Error()
	} else {
		var decodeErr error
		task, decodeErr = DecodeJSON[T](delivery.Task.Payload, w.kind, w.version)
		if decodeErr != nil {
			status = StatusFailed
			errorText = decodeErr.Error()
		} else if handlerErr := w.handler.Handle(workCtx, delivery, task); handlerErr != nil {
			status = StatusFailed
			errorText = handlerErr.Error()
		}
	}

	close(stopRenew)
	renewWG.Wait()
	select {
	case <-leaseLost:
		status = StatusCanceled
	default:
	}
	if runCtx.Err() != nil {
		status = StatusCanceled
	}

	retainUntil := time.Now().UTC().Add(w.shutdownTimeout)
	if err := w.complete(runCtx, CompleteRequest{
		ExecutionID: delivery.ExecutionID,
		LeaseToken:  delivery.LeaseToken,
		Status:      status,
		Error:       errorText,
		RetainUntil: &retainUntil,
	}); err != nil {
		// Complete is itself lease-fenced. A stale or missing lease is local
		// to this delivery even when Renew did not observe the loss first.
		if !errdefs.IsConflict(err) && !errdefs.IsNotFound(err) {
			report(err)
		}
	}
}

func (w *Worker[T]) complete(runCtx context.Context, request CompleteRequest) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), w.shutdownTimeout)
	defer cancel()

	var lastErr error
	backoff := w.retryInitial
	for {
		err := w.source.Complete(ctx, request)
		if err == nil {
			return nil
		}
		if permanentCompleteError(err) {
			return err
		}
		lastErr = err

		if !waitFor(ctx, backoff) {
			timeoutErr := errdefs.Timeout(fmt.Errorf(
				"scheduler: complete execution %q before shutdown timeout: %w",
				request.ExecutionID,
				ctx.Err(),
			))
			return errors.Join(lastErr, timeoutErr)
		}
		backoff = nextBackoff(backoff, w.retryMaximum)
	}
}

func permanentWorkerError(err error) bool {
	return errdefs.IsValidation(err) ||
		errdefs.IsNotFound(err) ||
		errdefs.IsConflict(err) ||
		errdefs.IsUnauthorized(err) ||
		errdefs.IsForbidden(err) ||
		errdefs.IsPolicyDenied(err) ||
		errdefs.IsBudgetExceeded(err) ||
		errdefs.IsInterrupted(err) ||
		errdefs.IsAborted(err)
}

func permanentCompleteError(err error) bool {
	return permanentWorkerError(err)
}

func nextBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum-current {
		return maximum
	}
	return min(current*2, maximum)
}

func waitFor(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func waitForAny(ctx context.Context, stop <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-stop:
		return false
	case <-ctx.Done():
		return false
	}
}
