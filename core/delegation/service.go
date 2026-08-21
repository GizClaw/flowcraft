package delegation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/telemetry"
	otellog "go.opentelemetry.io/otel/log"
)

const (
	defaultMaxConcurrency       = 4
	defaultMaxDepth             = 8
	defaultIdempotencyRetention = time.Hour

	workerRetryInitial     = 10 * time.Millisecond
	workerRetryMax         = 250 * time.Millisecond
	completeMaxAttempts    = 3
	completeAttemptTimeout = 100 * time.Millisecond
)

// AsyncRequest is the backend-neutral unit persisted by an AsyncBackend.
// Caller and Depth preserve delegation metadata across queue boundaries.
type AsyncRequest struct {
	Request Request `json:"request"`
	Caller  string  `json:"caller,omitempty"`
	Depth   int     `json:"depth"`
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

// AsyncBackend stores asynchronous delegations and reports their status. Submit
// must safely replay the same non-empty Request.IdempotencyKey and identical
// AsyncRequest during the backend's declared retention window, returning the
// same id; reuse with different request semantics must return a
// conflict-classified error. The service borrows an injected backend and never
// closes it.
type AsyncBackend interface {
	Submit(ctx context.Context, req AsyncRequest) (id string, err error)
	Status(ctx context.Context, id string) (Response, error)
}

// IdempotencyRetentionProvider optionally exposes an AsyncBackend's finite
// terminal replay and status-query window.
type IdempotencyRetentionProvider interface {
	IdempotencyRetention() time.Duration
}

// WorkSource is the optional worker side of an AsyncBackend. When implemented,
// Service starts bounded workers that claim backend-neutral work and complete
// it through the same backend. Claim must return a unique, non-empty lease
// token and unblock when ctx is canceled. Complete must only apply a terminal
// response when the token identifies the current lease; stale completions are
// ignored.
type WorkSource interface {
	Claim(ctx context.Context) (Work, error)
	Complete(ctx context.Context, id, leaseToken string, response Response) error
}

// Runner is the restricted execution seam a queue-owned worker may use.
type Runner interface {
	Run(ctx context.Context, req AsyncRequest) (Response, error)
}

// Option configures a local Service.
type Option func(*serviceConfig) error

type serviceConfig struct {
	maxConcurrency       int
	maxDepth             int
	timeout              time.Duration
	idempotencyRetention time.Duration
	workerHost           agent.Host
	sessionProvider      SessionProvider
	deferWorkers         bool
}

type idempotencyCall struct {
	request  Request
	done     chan struct{}
	response Response
	err      error
}

type idempotencyResult struct {
	request  Request
	response Response
	expires  time.Time
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

// WithIdempotencyRetention sets how long successful responses remain safely
// replayable. The retention must be positive.
func WithIdempotencyRetention(retention time.Duration) Option {
	return func(config *serviceConfig) error {
		if retention <= 0 {
			return errdefs.Validationf("local delegation: idempotency retention must be positive")
		}
		config.idempotencyRetention = retention
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

// WithSessionProvider sets the identity policy for delegated subagent
// sessions. When nil and a session manager is bound, the service mints a
// fresh ContextID per delegation.
func WithSessionProvider(provider SessionProvider) Option {
	return func(config *serviceConfig) error {
		if isNilInterface(provider) {
			return errdefs.Validationf("local delegation: session provider is nil")
		}
		config.sessionProvider = provider
		return nil
	}
}

// WithDeferredWorkers defers asynchronous worker startup until Start. This is
// useful when a lifecycle owner must finish binding all dependencies before
// background work begins.
func WithDeferredWorkers() Option {
	return func(config *serviceConfig) error {
		config.deferWorkers = true
		return nil
	}
}

// LocalService executes local sync delegations and coordinates optional async
// storage/workers.
type LocalService struct {
	directory      *LocalDirectory
	backend        AsyncBackend
	work           WorkSource
	maxConcurrency int
	maxDepth       int
	timeout        time.Duration
	slots          chan struct{}

	idempotencyRetention time.Duration
	asyncRetention       time.Duration
	idempotencyMu        sync.Mutex
	idempotencyCalls     map[string]*idempotencyCall
	idempotencyCache     map[string]idempotencyResult

	stateMu        sync.Mutex
	closed         bool
	workersStarted bool
	active         sync.WaitGroup

	workerCtx    context.Context
	cancelWorker context.CancelFunc
	workerHost   agent.Host
	// sessionProvider is the identity policy for subagent sessions. It is
	// immutable after construction.
	sessionProvider SessionProvider
	// sessionManager is bound by the runtime through ManagerBinder before
	// the service serves traffic; writes are serialized by stateMu.
	sessionManager *session.Manager
	workers        sync.WaitGroup

	closeOnce  sync.Once
	closeErr   error
	workerErrs []error
}

// NewService constructs a local service. Successful responses in every mode
// retain one shared idempotency key and request fingerprint. Async Accepted
// responses use the shorter of the service retention and a positive backend
// retention declaration, so replay cannot outlive backend queryability. If the
// backend also implements WorkSource, bounded workers start immediately. The
// backend remains owned by the deploy Result or caller.
func NewService(directory *LocalDirectory, backend AsyncBackend, opts ...Option) (*LocalService, error) {
	if directory == nil {
		return nil, errdefs.Validationf("local delegation: directory is nil")
	}
	if isNilInterface(backend) {
		backend = nil
	}
	config := serviceConfig{
		maxConcurrency:       defaultMaxConcurrency,
		maxDepth:             defaultMaxDepth,
		idempotencyRetention: defaultIdempotencyRetention,
		workerHost:           agent.NoopHost{},
	}
	for _, option := range opts {
		if option != nil {
			if err := option(&config); err != nil {
				return nil, err
			}
		}
	}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	asyncRetention := config.idempotencyRetention
	if provider, ok := backend.(IdempotencyRetentionProvider); ok && !isNilInterface(provider) {
		if retention := provider.IdempotencyRetention(); retention > 0 {
			asyncRetention = min(asyncRetention, retention)
		}
	}
	service := &LocalService{
		directory:            directory,
		backend:              backend,
		maxConcurrency:       config.maxConcurrency,
		maxDepth:             config.maxDepth,
		timeout:              config.timeout,
		sessionProvider:      config.sessionProvider,
		slots:                make(chan struct{}, config.maxConcurrency),
		idempotencyRetention: config.idempotencyRetention,
		asyncRetention:       asyncRetention,
		idempotencyCalls:     make(map[string]*idempotencyCall),
		idempotencyCache:     make(map[string]idempotencyResult),
		workerCtx:            workerCtx,
		cancelWorker:         cancelWorker,
	}
	service.workerHost = WithService(config.workerHost, service)
	service.workers.Add(1)
	go service.idempotencyJanitor()
	if source, ok := backend.(WorkSource); ok && !isNilInterface(source) {
		service.work = source
		if !config.deferWorkers {
			if err := service.Start(); err != nil {
				cancelWorker()
				service.workers.Wait()
				return nil, err
			}
		}
	}
	return service, nil
}

// BindSessionManager implements session.ManagerBinder: the runtime hands
// this service the session manager that owns subagent session lifecycle.
// Binding is set-once: a second bind is a conflict. The write is
// serialized by stateMu; reads happen through sessionManager.
func (s *LocalService) BindSessionManager(manager *session.Manager) error {
	if s == nil {
		return errdefs.Validationf("local delegation: nil service")
	}
	if manager == nil {
		return errdefs.Validationf("local delegation: session manager is nil")
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.sessionManager != nil {
		return errdefs.Conflictf(
			"local delegation: session manager is already bound")
	}
	s.sessionManager = manager
	return nil
}

// boundSessionManager returns the bound session manager, or nil when the
// runtime never bound one (legacy execution path).
func (s *LocalService) boundSessionManager() *session.Manager {
	if s == nil {
		return nil
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.sessionManager
}

// Start begins asynchronous workers when the backend supports WorkSource.
// Repeated calls are safe and do not create duplicate workers.
func (s *LocalService) Start() error {
	if s == nil {
		return errdefs.NotAvailablef("local delegation: nil service")
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return errdefs.NotAvailablef("local delegation: service is closed")
	}
	if s.work == nil || s.workersStarted {
		return nil
	}
	s.workersStarted = true
	for range s.maxConcurrency {
		s.workers.Add(1)
		go s.worker()
	}
	return nil
}

// Delegate implements core/delegation.Service.
func (s *LocalService) Delegate(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	req = cloneRequest(req)
	if err := s.begin(); err != nil {
		return Response{}, err
	}
	defer s.active.Done()

	if req.IdempotencyKey != "" {
		return s.delegateIdempotent(ctx, req)
	}
	return s.delegate(ctx, req)
}

func (s *LocalService) delegate(
	ctx context.Context,
	req Request,
) (Response, error) {
	if _, err := s.directory.Lookup(ctx, req.Target); err != nil {
		return Response{}, err
	}

	switch req.Mode {
	case ModeSync:
		meta := metadataFromContext(ctx)
		return s.runAt(ctx, AsyncRequest{
			Request: req,
			Caller:  meta.caller,
			Depth:   meta.depth + 1,
		}, meta.leased)
	case ModeAsync:
		if s.backend == nil {
			return Response{}, UnsupportedMode(req.Mode)
		}
		meta := metadataFromContext(ctx)
		depth := meta.depth + 1
		if err := s.checkDepth(depth); err != nil {
			return Response{}, err
		}
		id, err := s.backend.Submit(ctx, AsyncRequest{
			Request: cloneRequest(req),
			Caller:  meta.caller,
			Depth:   depth,
		})
		if err != nil {
			return Response{}, err
		}
		if id == "" {
			return Response{}, errdefs.Internalf("local delegation: async backend returned an empty id")
		}
		return Response{ID: id, Status: StatusAccepted}, nil
	default:
		return Response{}, UnsupportedMode(req.Mode)
	}
}

func (s *LocalService) delegateIdempotent(
	ctx context.Context,
	req Request,
) (Response, error) {
	key := req.IdempotencyKey
	s.idempotencyMu.Lock()
	s.expireIdempotencyResults(time.Now())
	if result, ok := s.idempotencyCache[key]; ok {
		if !sameRequest(result.request, req) {
			s.idempotencyMu.Unlock()
			return Response{}, idempotencyConflict(key)
		}
		response := cloneResponse(result.response)
		s.idempotencyMu.Unlock()
		return response, nil
	}
	if call, ok := s.idempotencyCalls[key]; ok {
		if !sameRequest(call.request, req) {
			s.idempotencyMu.Unlock()
			return Response{}, idempotencyConflict(key)
		}
		done := call.done
		s.idempotencyMu.Unlock()
		if ctx == nil {
			<-done
		} else {
			select {
			case <-ctx.Done():
				return Response{}, ctx.Err()
			case <-done:
			}
			if err := ctx.Err(); err != nil {
				return Response{}, err
			}
		}
		return cloneResponse(call.response), call.err
	}

	call := &idempotencyCall{
		request: cloneRequest(req),
		done:    make(chan struct{}),
	}
	s.idempotencyCalls[key] = call
	s.idempotencyMu.Unlock()

	response, err := s.delegate(ctx, req)
	s.idempotencyMu.Lock()
	delete(s.idempotencyCalls, key)
	call.response = cloneResponse(response)
	call.err = err
	if err == nil {
		s.cacheIdempotencyResult(key, call.request, call.response, time.Now())
	}
	close(call.done)
	s.idempotencyMu.Unlock()
	return cloneResponse(response), err
}

func (s *LocalService) cacheIdempotencyResult(
	key string,
	request Request,
	response Response,
	now time.Time,
) {
	retention := s.idempotencyRetention
	if request.Mode == ModeAsync {
		retention = s.asyncRetention
	}
	s.idempotencyCache[key] = idempotencyResult{
		request:  cloneRequest(request),
		response: cloneResponse(response),
		expires:  now.Add(retention),
	}
}

func (s *LocalService) expireIdempotencyResults(now time.Time) {
	for key, result := range s.idempotencyCache {
		if !now.Before(result.expires) {
			delete(s.idempotencyCache, key)
		}
	}
}

func (s *LocalService) idempotencyJanitor() {
	defer s.workers.Done()
	interval := min(s.idempotencyRetention, s.asyncRetention) / 2
	if interval > time.Minute {
		interval = time.Minute
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.workerCtx.Done():
			return
		case now := <-ticker.C:
			s.idempotencyMu.Lock()
			s.expireIdempotencyResults(now)
			s.idempotencyMu.Unlock()
		}
	}
}

func sameRequest(left, right Request) bool {
	return left.Mode == right.Mode &&
		left.Target == right.Target &&
		left.Input == right.Input &&
		left.IdempotencyKey == right.IdempotencyKey &&
		maps.Equal(left.Metadata, right.Metadata)
}

func idempotencyConflict(key string) error {
	return errdefs.Conflictf(
		"local delegation: idempotency key %q was already used for a different request",
		key,
	)
}

// Get returns a normalized backend status snapshot.
func (s *LocalService) Get(ctx context.Context, id string) (Response, error) {
	if id == "" {
		return Response{}, errdefs.Validationf("local delegation: delegation id is required")
	}
	if err := s.begin(); err != nil {
		return Response{}, err
	}
	defer s.active.Done()
	if s.backend == nil {
		return Response{}, RequestNotFound(id)
	}
	response, err := s.backend.Status(ctx, id)
	if err != nil {
		return Response{}, err
	}
	return normalizeResponse(id, response)
}

// Run executes a backend-neutral work item through the same depth, timeout,
// host-propagation, and concurrency limits as a sync call.
func (s *LocalService) Run(ctx context.Context, req AsyncRequest) (Response, error) {
	if err := req.Request.Validate(); err != nil {
		return Response{}, err
	}
	if req.Request.Mode != ModeAsync {
		return Response{}, errdefs.Validationf("local delegation runner: work mode must be %q", ModeAsync)
	}
	if err := s.begin(); err != nil {
		return Response{}, err
	}
	defer s.active.Done()
	return s.runAt(ctx, req, false)
}

// Close rejects new operations, cancels service-owned workers, and waits for
// every admitted operation and worker to finish. It is idempotent.
func (s *LocalService) Close() error {
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

func (s *LocalService) worker() {
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
			telemetry.Warn(s.workerCtx, "local delegation: claim work failed, will retry",
				otellog.Int("delegation.claim_failures", claimFailures),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
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
			response = Response{
				ID:     work.ID,
				Status: StatusFailed,
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

func (s *LocalService) complete(id, leaseToken string, response Response) error {
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

func (s *LocalService) recordWorkerError(err error) {
	if err == nil {
		return
	}
	telemetry.Error(context.Background(), "local delegation: worker stopped after error",
		otellog.String(telemetry.AttrErrorMessage, err.Error()))
	s.stateMu.Lock()
	s.workerErrs = append(s.workerErrs, err)
	s.closed = true
	s.stateMu.Unlock()
	s.cancelWorker()
}

func (s *LocalService) runClaimed(ctx context.Context, req AsyncRequest) (Response, error) {
	// A claim made before Close is service-owned work. workers.Wait provides
	// its lifecycle barrier, so it must not pass through begin after closed.
	return s.runAt(ctx, req, false)
}

func (s *LocalService) runAt(ctx context.Context, req AsyncRequest, reuseSlot bool) (Response, error) {
	if err := s.checkDepth(req.Depth); err != nil {
		return Response{}, err
	}
	instance, err := s.directory.Lookup(ctx, req.Request.Target)
	if err != nil {
		return Response{}, err
	}
	telemetry.Info(ctx, "local delegation: run started",
		otellog.String(telemetry.AttrAgentID, instance.ID),
		otellog.String(telemetry.AttrDelegationTarget, req.Request.Target),
		otellog.String(telemetry.AttrDelegationMode, string(req.Request.Mode)),
		otellog.Int(telemetry.AttrDelegationDepth, req.Depth),
		otellog.String(telemetry.AttrDelegationCaller, req.Caller),
	)

	// Identity rule: with no provider and no bound manager the ContextID
	// stays empty (legacy, fully compatible); with a provider, or once a
	// manager is bound, a ContextID is always set.
	manager := s.boundSessionManager()
	key, hasKey, persistent := session.Key{}, false, false
	if provider := s.sessionProvider; provider != nil {
		contextID, err := provider.CreateContextID(ctx, req)
		if err != nil {
			return Response{}, err
		}
		if strings.TrimSpace(contextID) == "" {
			return Response{}, errdefs.Validationf(
				"local delegation: session provider returned an empty context id")
		}
		key = session.Key{AgentID: req.Request.Target, ContextID: contextID}
		hasKey = true
		persistent = provider.Persistent()
	}
	if manager == nil {
		return s.runAtLegacy(ctx, req, reuseSlot, key, hasKey, instance)
	}
	if !hasKey {
		key = session.Key{AgentID: req.Request.Target, ContextID: newContextID()}
	}

	// Timeout and delegation metadata (caller/depth) both hang off
	// execCtx; Start must see them so nested delegations keep their depth
	// and the service timeout bounds the subagent turn.
	execCtx := context.WithValue(ctx, delegationContextKey{}, delegationMetadata{
		caller: req.Request.Target,
		depth:  req.Depth,
		leased: true,
	})
	cancel := func() {}
	if s.timeout > 0 {
		execCtx, cancel = context.WithTimeout(execCtx, s.timeout)
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

	lease, err := manager.GetOrCreate(execCtx, key)
	if err != nil {
		return Response{}, err
	}
	defer func() {
		if cerr := lease.Close(); cerr != nil {
			telemetry.WarnErr(execCtx, "local delegation: close session lease failed", cerr,
				otellog.String(telemetry.AttrAgentID, key.AgentID),
				otellog.String(telemetry.AttrConversationID, key.ContextID))
		}
	}()

	turn, err := lease.Session().StartWithOptions(execCtx, agent.Request{
		ContextID: key.ContextID,
		Message:   message.NewTextMessage(message.RoleUser, req.Request.Input),
		Inputs:    metadataInputs(req),
	}, s.delegationStartOptions(persistent)...)
	if err != nil {
		return Response{}, err
	}

	result, err := waitTurnCancelOnDone(execCtx, turn)
	if err != nil {
		return canceledOrFailedResponse(err), nil
	}
	return responseFromTurn(result, key.ContextID), nil
}

// runAtLegacy executes a delegated run without session lifecycle: plain
// agent.Execute with an empty ContextID unless the identity policy
// supplied one. It keeps the historical host-propagation behavior (sync
// inherits the caller host, async uses the worker host).
func (s *LocalService) runAtLegacy(
	ctx context.Context,
	req AsyncRequest,
	reuseSlot bool,
	key session.Key,
	hasKey bool,
	instance *agent.Agent,
) (Response, error) {
	if err := s.checkDepth(req.Depth); err != nil {
		return Response{}, err
	}

	execCtx := context.WithValue(ctx, delegationContextKey{}, delegationMetadata{
		caller: req.Request.Target,
		depth:  req.Depth,
		leased: true,
	})
	cancel := func() {}
	if s.timeout > 0 {
		execCtx, cancel = context.WithTimeout(execCtx, s.timeout)
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
		Message: message.NewTextMessage(message.RoleUser, req.Request.Input),
	}
	if hasKey {
		agentRequest.ContextID = key.ContextID
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
	result, err := agent.Execute(execCtx, *instance, nil, agentRequest, options...)
	if err != nil {
		if execCtx.Err() != nil {
			return canceledResponse("", execCtx.Err()), nil
		}
		return Response{}, err
	}
	return responseFromAgent(result), nil
}

func (s *LocalService) begin() error {
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

func (s *LocalService) checkDepth(depth int) error {
	if depth <= 0 {
		return errdefs.Validationf("local delegation: depth must be positive")
	}
	if depth > s.maxDepth {
		return errdefs.PolicyDeniedf(
			"local delegation: maximum depth %d exceeded at depth %d",
			s.maxDepth, depth)
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

func responseFromAgent(result *agent.Result) Response {
	if result == nil {
		return Response{
			ID:     newID(),
			Status: StatusFailed,
			Error:  "agent returned no result",
		}
	}
	id := result.RunID
	if id == "" {
		id = newID()
	}
	response := Response{ID: id}
	switch result.Status {
	case agent.StatusCompleted:
		response.Status = StatusSucceeded
		response.Output = result.Text()
	case agent.StatusInterrupted, agent.StatusCanceled:
		response.Status = StatusCanceled
		if result.Err != nil {
			response.Error = result.Err.Error()
		}
	default:
		response.Status = StatusFailed
		if result.Err != nil {
			response.Error = result.Err.Error()
		} else {
			response.Error = fmt.Sprintf("agent execution ended with status %q", result.Status)
		}
	}
	return response
}

// responseFromTurn maps a session turn's terminal result to a delegation
// response, mirroring responseFromAgent and backfilling the subagent
// session identity for boards and resume tooling.
func responseFromTurn(result *agent.Result, contextID string) Response {
	response := responseFromAgent(result)
	if response.Metadata == nil {
		response.Metadata = make(map[string]string, 1)
	}
	response.Metadata["delegation.session_id"] = contextID
	return response
}

// delegationStartOptions builds the session options for a delegated
// subagent turn: questions to the user are refused (never block), and
// non-persistent identities run the turn ephemeral so no session state or
// run checkpoint is ever written.
func (s *LocalService) delegationStartOptions(persistent bool) []session.StartOption {
	options := []session.StartOption{
		session.WithAskUserOverride(refuseSubagentAskUser),
	}
	if !persistent {
		options = append(options, session.WithEphemeral())
	}
	return options
}

// refuseSubagentAskUser is the default subagent asker: subagents never
// interrupt the user, so questions fail fast instead of blocking forever
// on an unattended prompt.
func refuseSubagentAskUser(context.Context, agent.UserPrompt) (agent.UserReply, error) {
	return agent.UserReply{}, errdefs.NotAvailablef(
		"local delegation: subagent cannot ask the user")
}

// turnCancelWaitTimeout bounds how long a canceled delegation turn may
// take to reach its terminal state before the caller gives up.
const turnCancelWaitTimeout = 5 * time.Second

// waitTurnCancelOnDone waits for a turn's terminal result. On ctx
// cancellation it explicitly cancels the turn and waits again with a
// fresh context (Turn.Wait with the canceled ctx would return
// immediately), so the caller maps a settled canceled result instead of
// leaking the raw context error. A turn that already settled with a real
// terminal error (e.g. a seed or referee infrastructure failure) is
// final and is returned as-is, never relabeled as a canceled wait.
func waitTurnCancelOnDone(ctx context.Context, turn *session.Turn) (*agent.Result, error) {
	result, err := turn.Wait(ctx)
	if err == nil {
		return result, nil
	}
	if isTerminalTurn(turn) {
		waitCtx, cancel := context.WithTimeout(
			context.Background(), turnCancelWaitTimeout)
		defer cancel()
		return turn.Wait(waitCtx)
	}
	turn.Cancel()
	waitCtx, cancel := context.WithTimeout(context.Background(), turnCancelWaitTimeout)
	defer cancel()
	result, err = turn.Wait(waitCtx)
	if err != nil {
		return nil, fmt.Errorf(
			"local delegation: wait for canceled turn: %w", err)
	}
	return result, nil
}

// isTerminalTurn reports whether the turn reached a settled terminal
// state. session.TurnState's terminal predicate is unexported, so the
// stable exported state constants are checked here.
func isTerminalTurn(turn *session.Turn) bool {
	switch turn.State() {
	case session.TurnCompleted, session.TurnInterrupted,
		session.TurnCanceled, session.TurnFailed, session.TurnAborted:
		return true
	default:
		return false
	}
}

// canceledOrFailedResponse classifies a session-path failure: context
// cancellation or timeout maps to a canceled response; anything else
// (e.g. a session that closed mid-run) maps to a failed response.
func canceledOrFailedResponse(err error) Response {
	if errdefs.IsAborted(err) || errdefs.IsTimeout(err) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return canceledResponse("", err)
	}
	return Response{ID: newID(), Status: StatusFailed, Error: err.Error()}
}

// metadataInputs converts request metadata into agent request inputs.
func metadataInputs(req AsyncRequest) map[string]any {
	if len(req.Request.Metadata) == 0 {
		return nil
	}
	inputs := make(map[string]any, len(req.Request.Metadata))
	for key, value := range req.Request.Metadata {
		inputs[key] = value
	}
	return inputs
}

func canceledResponse(id string, cause error) Response {
	if id == "" {
		id = newID()
	}
	response := Response{ID: id, Status: StatusCanceled}
	if cause != nil {
		response.Error = cause.Error()
	}
	return response
}

func normalizeResponse(id string, response Response) (Response, error) {
	response.ID = id
	response.Metadata = cloneMetadata(response.Metadata)
	switch response.Status {
	case StatusAccepted, StatusRunning:
		response.Output = ""
		response.Error = ""
	case StatusSucceeded:
		response.Error = ""
	case StatusFailed:
		if response.Error == "" {
			response.Error = "delegation failed"
		}
	case StatusCanceled:
		response.Output = ""
	default:
		return Response{}, errdefs.Internalf(
			"local delegation: async backend returned invalid status %q", response.Status)
	}
	if err := response.Validate(); err != nil {
		return Response{}, errdefs.Internal(fmt.Errorf(
			"local delegation: invalid async backend response: %w", err))
	}
	return response, nil
}

func cloneRequest(req Request) Request {
	req.Metadata = cloneMetadata(req.Metadata)
	return req
}

func cloneResponse(response Response) Response {
	response.Metadata = cloneMetadata(response.Metadata)
	return response
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
	_ Service = (*LocalService)(nil)
	_ Runner  = (*LocalService)(nil)
)
