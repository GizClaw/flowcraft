package delegation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

const (
	defaultMaxConcurrency = 4
	defaultMaxDepth       = 8

	workerRetryInitial     = 10 * time.Millisecond
	workerRetryMax         = 250 * time.Millisecond
	completeMaxAttempts    = 3
	completeAttemptTimeout = 100 * time.Millisecond
)

// AsyncRequest is the backend-neutral unit persisted by an AsyncBackend.
// Caller and Depth preserve delegation metadata across queue boundaries.
type AsyncRequest struct {
	Request sdkdelegation.Request `json:"request"`
	Caller  string                `json:"caller,omitempty"`
	Depth   int                   `json:"depth"`
}

// Work is one claimed asynchronous request. LeaseToken uniquely identifies
// this claim generation and must be supplied to WorkSource.Complete. Context
// optionally carries the backend-owned execution lease; canceling it must stop
// the claimed execution.
type Work struct {
	ID         string          `json:"id"`
	LeaseToken string          `json:"lease_token"`
	Request    AsyncRequest    `json:"request"`
	Context    context.Context `json:"-"`
}

// AsyncBackend stores asynchronous delegations and reports their status. The
// service borrows an injected backend and never closes it.
type AsyncBackend interface {
	Submit(ctx context.Context, req AsyncRequest) (id string, err error)
	Status(ctx context.Context, id string) (sdkdelegation.Response, error)
}

// WorkSource is the optional worker side of an AsyncBackend. When implemented,
// Service starts bounded workers that claim backend-neutral work and complete
// it through the same backend. Claim must return a unique, non-empty lease
// token and unblock when ctx is canceled. Complete must only apply a terminal
// response when the token identifies the current lease; stale completions are
// ignored.
type WorkSource interface {
	Claim(ctx context.Context) (Work, error)
	Complete(ctx context.Context, id, leaseToken string, response sdkdelegation.Response) error
}

// Runner is the restricted execution seam a queue-owned worker may use.
type Runner interface {
	Run(ctx context.Context, req AsyncRequest) (sdkdelegation.Response, error)
}

// Option configures a local Service.
type Option func(*serviceConfig) error

type serviceConfig struct {
	maxConcurrency int
	maxDepth       int
	timeout        time.Duration
	workerHost     agent.Host
}

// WithMaxConcurrency bounds all local agent executions, including sync calls
// and work claimed by asynchronous workers.
func WithMaxConcurrency(limit int) Option {
	return func(config *serviceConfig) error {
		if limit <= 0 {
			return errdefs.Validationf("local delegation: max concurrency must be positive")
		}
		config.maxConcurrency = limit
		return nil
	}
}

// WithMaxDepth sets the largest allowed delegation depth. A top-level
// delegation executes at depth one.
func WithMaxDepth(limit int) Option {
	return func(config *serviceConfig) error {
		if limit <= 0 {
			return errdefs.Validationf("local delegation: max depth must be positive")
		}
		config.maxDepth = limit
		return nil
	}
}

// WithTimeout caps each local agent execution. Zero leaves the caller's
// deadline unchanged.
func WithTimeout(timeout time.Duration) Option {
	return func(config *serviceConfig) error {
		if timeout < 0 {
			return errdefs.Validationf("local delegation: timeout cannot be negative")
		}
		config.timeout = timeout
		return nil
	}
}

// WithWorkerHost sets the stable base Host used by asynchronous workers.
// Service adds its delegation capability without replacing optional
// capabilities such as EventBusProvider. The caller retains Host ownership.
func WithWorkerHost(host agent.Host) Option {
	return func(config *serviceConfig) error {
		if isNilInterface(host) {
			return errdefs.Validationf("local delegation: worker host is nil")
		}
		config.workerHost = host
		return nil
	}
}

// Service executes local sync delegations and coordinates optional async
// storage/workers.
type Service struct {
	directory *Directory
	backend   AsyncBackend
	work      WorkSource
	config    serviceConfig
	slots     chan struct{}

	stateMu sync.Mutex
	closed  bool
	active  sync.WaitGroup

	workerCtx    context.Context
	cancelWorker context.CancelFunc
	workerHost   agent.Host
	workers      sync.WaitGroup

	closeOnce  sync.Once
	closeErr   error
	workerErrs []error
}

// NewService constructs a local service. If backend also implements
// WorkSource, bounded workers start immediately. The backend remains owned by
// the deploy Result or caller.
func NewService(directory *Directory, backend AsyncBackend, opts ...Option) (*Service, error) {
	if directory == nil {
		return nil, errdefs.Validationf("local delegation: directory is nil")
	}
	if isNilInterface(backend) {
		backend = nil
	}
	config := serviceConfig{
		maxConcurrency: defaultMaxConcurrency,
		maxDepth:       defaultMaxDepth,
		workerHost:     agent.NoopHost{},
	}
	for _, option := range opts {
		if option != nil {
			if err := option(&config); err != nil {
				return nil, err
			}
		}
	}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	service := &Service{
		directory:    directory,
		backend:      backend,
		config:       config,
		slots:        make(chan struct{}, config.maxConcurrency),
		workerCtx:    workerCtx,
		cancelWorker: cancelWorker,
	}
	service.workerHost = sdkdelegation.WithService(config.workerHost, service)
	if source, ok := backend.(WorkSource); ok && !isNilInterface(source) {
		service.work = source
		for range config.maxConcurrency {
			service.workers.Add(1)
			go service.worker()
		}
	}
	return service, nil
}

// Delegate implements sdk/delegation.Service.
func (s *Service) Delegate(ctx context.Context, req sdkdelegation.Request) (sdkdelegation.Response, error) {
	if err := req.Validate(); err != nil {
		return sdkdelegation.Response{}, err
	}
	if err := s.begin(); err != nil {
		return sdkdelegation.Response{}, err
	}
	defer s.active.Done()

	if _, err := s.directory.Lookup(ctx, req.Target); err != nil {
		return sdkdelegation.Response{}, err
	}

	switch req.Mode {
	case sdkdelegation.ModeSync:
		meta := metadataFromContext(ctx)
		return s.runAt(ctx, AsyncRequest{
			Request: req,
			Caller:  meta.caller,
			Depth:   meta.depth + 1,
		}, meta.leased)
	case sdkdelegation.ModeHandoff:
		return sdkdelegation.Response{
			ID:     newID(),
			Status: sdkdelegation.StatusAccepted,
			Metadata: map[string]string{
				"control_transfer": "true",
				"target":           req.Target,
			},
		}, nil
	case sdkdelegation.ModeAsync:
		if s.backend == nil {
			return sdkdelegation.Response{}, sdkdelegation.UnsupportedMode(req.Mode)
		}
		meta := metadataFromContext(ctx)
		depth := meta.depth + 1
		if err := s.checkDepth(depth); err != nil {
			return sdkdelegation.Response{}, err
		}
		id, err := s.backend.Submit(ctx, AsyncRequest{
			Request: cloneRequest(req),
			Caller:  meta.caller,
			Depth:   depth,
		})
		if err != nil {
			return sdkdelegation.Response{}, err
		}
		if id == "" {
			return sdkdelegation.Response{}, errdefs.Internalf("local delegation: async backend returned an empty id")
		}
		return sdkdelegation.Response{ID: id, Status: sdkdelegation.StatusAccepted}, nil
	default:
		return sdkdelegation.Response{}, sdkdelegation.UnsupportedMode(req.Mode)
	}
}

// Get returns a normalized backend status snapshot.
func (s *Service) Get(ctx context.Context, id string) (sdkdelegation.Response, error) {
	if id == "" {
		return sdkdelegation.Response{}, errdefs.Validationf("local delegation: delegation id is required")
	}
	if err := s.begin(); err != nil {
		return sdkdelegation.Response{}, err
	}
	defer s.active.Done()
	if s.backend == nil {
		return sdkdelegation.Response{}, sdkdelegation.RequestNotFound(id)
	}
	response, err := s.backend.Status(ctx, id)
	if err != nil {
		return sdkdelegation.Response{}, err
	}
	return normalizeResponse(id, response)
}

// Run executes a backend-neutral work item through the same depth, timeout,
// host-propagation, and concurrency limits as a sync call.
func (s *Service) Run(ctx context.Context, req AsyncRequest) (sdkdelegation.Response, error) {
	if err := req.Request.Validate(); err != nil {
		return sdkdelegation.Response{}, err
	}
	if req.Request.Mode != sdkdelegation.ModeAsync {
		return sdkdelegation.Response{}, errdefs.Validationf("local delegation runner: work mode must be %q", sdkdelegation.ModeAsync)
	}
	if err := s.begin(); err != nil {
		return sdkdelegation.Response{}, err
	}
	defer s.active.Done()
	return s.runAt(ctx, req, false)
}

// Close rejects new operations, cancels service-owned workers, and waits for
// every admitted operation and worker to finish. It is idempotent.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.stateMu.Lock()
		s.closed = true
		s.stateMu.Unlock()
		s.cancelWorker()
		s.workers.Wait()
		s.active.Wait()
		s.stateMu.Lock()
		s.closeErr = errors.Join(s.workerErrs...)
		s.stateMu.Unlock()
	})
	return s.closeErr
}

func (s *Service) worker() {
	defer s.workers.Done()
	claimFailures := 0
	for {
		work, err := s.work.Claim(s.workerCtx)
		if err != nil {
			if s.workerCtx.Err() != nil {
				return
			}
			if errdefs.IsNotAvailable(err) {
				return
			}
			if !waitForRetry(s.workerCtx, retryDelay(claimFailures)) {
				return
			}
			claimFailures++
			continue
		}
		claimFailures = 0

		workCtx, cancelWork := claimedWorkContext(s.workerCtx, work.Context)
		workCtx = agent.ContextWithHost(workCtx, s.workerHost)
		response, runErr := s.runClaimed(workCtx, work.Request)
		if workCtx.Err() != nil {
			response = canceledResponse(work.ID, workCtx.Err())
			runErr = nil
		}
		cancelWork()
		if runErr != nil {
			response = sdkdelegation.Response{
				ID:     work.ID,
				Status: sdkdelegation.StatusFailed,
				Error:  runErr.Error(),
			}
		}
		response.ID = work.ID
		if err := s.complete(work.ID, work.LeaseToken, response); err != nil {
			s.recordWorkerError(err)
			return
		}
	}
}

func (s *Service) complete(id, leaseToken string, response sdkdelegation.Response) error {
	var errs []error
	for attempt := range completeMaxAttempts {
		ctx, cancel := context.WithTimeout(context.Background(), completeAttemptTimeout)
		err := s.work.Complete(ctx, id, leaseToken, response)
		cancel()
		if err == nil {
			return nil
		}
		errs = append(errs, err)
		if attempt+1 < completeMaxAttempts {
			time.Sleep(retryDelay(attempt))
		}
	}
	return fmt.Errorf("local delegation: complete work %q: %w", id, errors.Join(errs...))
}

func (s *Service) recordWorkerError(err error) {
	if err == nil {
		return
	}
	s.stateMu.Lock()
	s.workerErrs = append(s.workerErrs, err)
	s.closed = true
	s.stateMu.Unlock()
	s.cancelWorker()
}

func (s *Service) runClaimed(ctx context.Context, req AsyncRequest) (sdkdelegation.Response, error) {
	// A claim made before Close is service-owned work. workers.Wait provides
	// its lifecycle barrier, so it must not pass through begin after closed.
	return s.runAt(ctx, req, false)
}

func (s *Service) runAt(ctx context.Context, req AsyncRequest, reuseSlot bool) (sdkdelegation.Response, error) {
	if err := s.checkDepth(req.Depth); err != nil {
		return sdkdelegation.Response{}, err
	}
	instance, err := s.directory.Lookup(ctx, req.Request.Target)
	if err != nil {
		return sdkdelegation.Response{}, err
	}

	execCtx := context.WithValue(ctx, delegationContextKey{}, delegationMetadata{
		caller: req.Request.Target,
		depth:  req.Depth,
		leased: true,
	})
	cancel := func() {}
	if s.config.timeout > 0 {
		execCtx, cancel = context.WithTimeout(execCtx, s.config.timeout)
	}
	defer cancel()

	if !reuseSlot {
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		case <-execCtx.Done():
			return canceledResponse("", execCtx.Err()), nil
		}
	}

	agentRequest := agent.Request{
		Message: inference.NewTextMessage(inference.RoleUser, req.Request.Input),
	}
	if len(req.Request.Metadata) > 0 {
		agentRequest.Inputs = make(map[string]any, len(req.Request.Metadata))
		for key, value := range req.Request.Metadata {
			agentRequest.Inputs[key] = value
		}
	}
	var options []agent.ExecuteOption
	if host, ok := agent.HostFromContext(execCtx); ok {
		options = append(options, agent.WithHost(host))
	}
	result, err := instance.Execute(execCtx, agentRequest, options...)
	if err != nil {
		if execCtx.Err() != nil {
			return canceledResponse("", execCtx.Err()), nil
		}
		return sdkdelegation.Response{}, err
	}
	return responseFromAgent(result), nil
}

func (s *Service) begin() error {
	if s == nil {
		return errdefs.NotAvailablef("local delegation: nil service")
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return errdefs.NotAvailablef("local delegation: service is closed")
	}
	s.active.Add(1)
	return nil
}

func (s *Service) checkDepth(depth int) error {
	if depth <= 0 {
		return errdefs.Validationf("local delegation: depth must be positive")
	}
	if depth > s.config.maxDepth {
		return errdefs.PolicyDeniedf(
			"local delegation: maximum depth %d exceeded at depth %d",
			s.config.maxDepth, depth)
	}
	return nil
}

type delegationContextKey struct{}

type delegationMetadata struct {
	caller string
	depth  int
	leased bool
}

func metadataFromContext(ctx context.Context) delegationMetadata {
	if ctx == nil {
		return delegationMetadata{}
	}
	metadata, _ := ctx.Value(delegationContextKey{}).(delegationMetadata)
	return metadata
}

func responseFromAgent(result *agent.Result) sdkdelegation.Response {
	if result == nil {
		return sdkdelegation.Response{
			ID:     newID(),
			Status: sdkdelegation.StatusFailed,
			Error:  "agent returned no result",
		}
	}
	id := result.RunID
	if id == "" {
		id = newID()
	}
	response := sdkdelegation.Response{ID: id}
	switch result.Status {
	case agent.StatusCompleted:
		response.Status = sdkdelegation.StatusSucceeded
		response.Output = result.Text()
	case agent.StatusInterrupted, agent.StatusCanceled:
		response.Status = sdkdelegation.StatusCanceled
		if result.Err != nil {
			response.Error = result.Err.Error()
		}
	default:
		response.Status = sdkdelegation.StatusFailed
		if result.Err != nil {
			response.Error = result.Err.Error()
		} else {
			response.Error = fmt.Sprintf("agent execution ended with status %q", result.Status)
		}
	}
	return response
}

func canceledResponse(id string, cause error) sdkdelegation.Response {
	if id == "" {
		id = newID()
	}
	response := sdkdelegation.Response{ID: id, Status: sdkdelegation.StatusCanceled}
	if cause != nil {
		response.Error = cause.Error()
	}
	return response
}

func normalizeResponse(id string, response sdkdelegation.Response) (sdkdelegation.Response, error) {
	response.ID = id
	response.Metadata = cloneMetadata(response.Metadata)
	switch response.Status {
	case sdkdelegation.StatusAccepted, sdkdelegation.StatusRunning:
		response.Output = ""
		response.Error = ""
	case sdkdelegation.StatusSucceeded:
		response.Error = ""
	case sdkdelegation.StatusFailed:
		if response.Error == "" {
			response.Error = "delegation failed"
		}
	case sdkdelegation.StatusCanceled:
		response.Output = ""
	default:
		return sdkdelegation.Response{}, errdefs.Internalf(
			"local delegation: async backend returned invalid status %q", response.Status)
	}
	if err := response.Validate(); err != nil {
		return sdkdelegation.Response{}, errdefs.Internal(fmt.Errorf(
			"local delegation: invalid async backend response: %w", err))
	}
	return response, nil
}

func cloneRequest(req sdkdelegation.Request) sdkdelegation.Request {
	req.Metadata = cloneMetadata(req.Metadata)
	return req
}

func cloneMetadata(metadata map[string]string) map[string]string {
	return maps.Clone(metadata)
}

func newID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return "delegation-" + hex.EncodeToString(bytes[:])
	}
	return fmt.Sprintf("delegation-%d", time.Now().UnixNano())
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

func claimedWorkContext(workerCtx, workCtx context.Context) (context.Context, context.CancelFunc) {
	if workCtx == nil {
		workCtx = workerCtx
	}
	ctx, cancel := context.WithCancel(workCtx)
	if workerCtx == workCtx {
		return ctx, cancel
	}
	stop := context.AfterFunc(workerCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func retryDelay(failures int) time.Duration {
	delay := workerRetryInitial
	for range failures {
		if delay >= workerRetryMax/2 {
			return workerRetryMax
		}
		delay *= 2
	}
	return delay
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

var (
	_ sdkdelegation.Service = (*Service)(nil)
	_ Runner                = (*Service)(nil)
)
