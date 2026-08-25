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
	"github.com/GizClaw/flowcraft/core/tool"
	otellog "go.opentelemetry.io/otel/log"
)

const (
	defaultMaxConcurrency       = 4
	defaultMaxDepth             = 8
	defaultIdempotencyRetention = time.Hour
	// defaultStreamEscrowTTL bounds how long an async delegation's
	// stream escrow survives without a claim; claimed entries are
	// refreshed and released at terminal completion, so the TTL is a
	// leak backstop, not a run-duration cap.
	defaultStreamEscrowTTL = time.Hour

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

	// ParentRunID is the run id of the delegating agent's run. Sync
	// delegations derive it from the ambient execution context;
	// async delegations persist it here so it survives the queue.
	ParentRunID string `json:"parent_run_id,omitempty"`

	// CallID is the delegate tool call id that started the
	// delegation. Like ParentRunID it is derived from the caller's
	// execution context and persisted across the queue for async.
	CallID string `json:"call_id,omitempty"`

	// Stream is the queue-crossing reference to the caller's stream
	// sinks. Nil means the delegation carries no live stream (the
	// caller had no inheritable sinks, or the deployment does not
	// support async streaming).
	Stream *StreamRef `json:"stream,omitempty"`
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
	streamEscrowTTL      time.Duration
	workerHost           agent.Host
	sessionProvider      SessionProvider
	streamResolver       StreamTargetResolver
	streamExporter       StreamTargetExporter
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

// streamEscrowEntry is one async delegation's stream attachment: the
// caller's live sinks (already downgraded to observers) plus a claim
// backstop deadline.
type streamEscrowEntry struct {
	specs   []session.SinkSpec
	expires time.Time
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

// WithStreamEscrowTTL bounds how long an async delegation's stream
// escrow may sit unclaimed (or between claims) before the service
// drops it. Claimed entries are refreshed and released at terminal
// completion, so a long-running job is unaffected by a short TTL; the
// TTL only guards abandoned entries. Zero uses the service default.
func WithStreamEscrowTTL(ttl time.Duration) Option {
	return func(config *serviceConfig) error {
		if ttl < 0 {
			return errdefs.Validationf("local delegation: stream escrow ttl cannot be negative")
		}
		config.streamEscrowTTL = ttl
		return nil
	}
}

// WithStreamTargetResolver registers the worker-side resolver that
// materializes a serializable [StreamTarget] (persisted in an async
// AsyncRequest) into a live stream sink. Resolvers MUST whitelist
// target kinds; the resolver is only consulted when no in-process
// escrow entry exists for the request.
func WithStreamTargetResolver(resolver StreamTargetResolver) Option {
	return func(config *serviceConfig) error {
		if resolver == nil {
			return errdefs.Validationf("local delegation: stream target resolver is nil")
		}
		config.streamResolver = resolver
		return nil
	}
}

// WithStreamTargetExporter registers the caller-side exporter that
// describes live sinks as serializable [StreamTarget]s on async
// submit. When set, the exporter is consulted for each inherited sink
// and the first describable target is persisted alongside the escrow
// ref, so cross-process workers can re-materialize the destination
// through [WithStreamTargetResolver] even when no in-process escrow
// entry survives.
func WithStreamTargetExporter(exporter StreamTargetExporter) Option {
	return func(config *serviceConfig) error {
		if exporter == nil {
			return errdefs.Validationf("local delegation: stream target exporter is nil")
		}
		config.streamExporter = exporter
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
	// streamEscrow holds the caller's live (downgraded) sink specs for
	// in-flight async delegations, keyed by the StreamRef.Ref carried
	// in the persisted AsyncRequest. Entries are released at terminal
	// completion, refreshed on claim, and swept by TTL as a backstop.
	streamEscrowMu  sync.Mutex
	streamEscrow    map[string]streamEscrowEntry
	streamEscrowTTL time.Duration
	streamResolver  StreamTargetResolver
	streamExporter  StreamTargetExporter
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
	if config.streamEscrowTTL <= 0 {
		config.streamEscrowTTL = defaultStreamEscrowTTL
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
		streamEscrow:         make(map[string]streamEscrowEntry),
		streamEscrowTTL:      config.streamEscrowTTL,
		streamResolver:       config.streamResolver,
		streamExporter:       config.streamExporter,
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
		parentRunID, callID := lineageFromContext(ctx)
		return s.runAt(ctx, AsyncRequest{
			Request:     req,
			Caller:      meta.caller,
			Depth:       meta.depth + 1,
			ParentRunID: parentRunID,
			CallID:      callID,
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
		parentRunID, callID := lineageFromContext(ctx)
		asyncReq := AsyncRequest{
			Request:     cloneRequest(req),
			Caller:      meta.caller,
			Depth:       depth,
			ParentRunID: parentRunID,
			CallID:      callID,
		}
		// Surface lineage on operational views (kanban cards,
		// delegation_status) through the portable metadata channel;
		// the injected keys are service-owned, not user input.
		asyncReq.Request.Metadata = injectDelegationLineage(
			asyncReq.Request.Metadata, parentRunID, callID)
		if specs, ok := s.asyncStreamSpecs(ctx); ok {
			ref := "escrow-" + newID()
			release := s.storeStreamEscrow(ref, specs)
			stream := &StreamRef{
				Ref:    ref,
				Policy: streamPolicySnapshotOf(specs[0]),
			}
			if target, spec, ok := s.exportStreamTarget(ctx, specs); ok {
				stream.Target = &target
				// The persisted policy describes the same sink the
				// target came from, so the worker re-materializes the
				// attachment with matching tuning.
				stream.Policy = streamPolicySnapshotOf(spec)
			}
			asyncReq.Stream = stream
			id, err := s.backend.Submit(ctx, asyncReq)
			if err != nil {
				release()
				return Response{}, err
			}
			if id == "" {
				release()
				return Response{}, errdefs.Internalf(
					"local delegation: async backend returned an empty id")
			}
			return Response{ID: id, Status: StatusAccepted}, nil
		}
		id, err := s.backend.Submit(ctx, asyncReq)
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
		s.streamEscrowMu.Lock()
		s.streamEscrow = nil
		s.streamEscrowMu.Unlock()
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
		s.releaseStreamEscrow(streamRefOf(work.Request.Stream))
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
			select {
			case <-s.workerCtx.Done():
				return fmt.Errorf(
					"local delegation: complete work %q: %w",
					id, errors.Join(errs...))
			case <-time.After(retryDelay(attempt)):
			}
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
	// Async work carries its stream attachment through the queue; the
	// worker restores the caller's sinks (or a resolver target) as an
	// inheritable policy before the session turn starts.
	ctx = s.inheritAsyncStreams(ctx, req)
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

	opts := s.delegationStartOptions(execCtx, persistent)
	parentRunID, callID := lineageFromContext(execCtx)
	if req.ParentRunID != "" {
		parentRunID = req.ParentRunID
	}
	if req.CallID != "" {
		callID = req.CallID
	}
	request := agent.Request{
		ContextID:   key.ContextID,
		Message:     message.NewTextMessage(message.RoleUser, req.Request.Input),
		Inputs:      metadataInputs(req),
		ParentRunID: parentRunID,
		Attributes:  lineageAttributes(callID),
	}

	// Resume path: a persistent session retried with the identical
	// request replays the parked run from its checkpoint instead of
	// starting fresh. Resume is best-effort for a retry: any failure to
	// probe or replay — transient checkpoint-store errors, unreadable
	// parked state, a missing checkpoint, or an engine that cannot
	// resume — degrades to a fresh Start with a warning rather than
	// failing the retry. Start re-validates the session and surfaces
	// real failures.
	var turn *session.Turn
	if persistent {
		parked, ok, err := lease.Session().ParkedRequest(execCtx)
		if err != nil {
			telemetry.WarnErr(execCtx,
				"local delegation: cannot inspect parked run, starting fresh", err,
				otellog.String(telemetry.AttrAgentID, key.AgentID),
				otellog.String(telemetry.AttrConversationID, key.ContextID))
		} else if ok && sameDelegationRequest(request, parked) {
			if resumed, err := lease.Session().ResumeWithOptions(execCtx, opts...); err == nil {
				turn = resumed
			} else {
				telemetry.WarnErr(execCtx,
					"local delegation: resume parked run failed, starting fresh", err,
					otellog.String(telemetry.AttrAgentID, key.AgentID),
					otellog.String(telemetry.AttrConversationID, key.ContextID))
			}
		}
	}
	if turn == nil {
		var err error
		turn, err = lease.Session().StartWithOptions(execCtx, request, opts...)
		if err != nil {
			return Response{}, err
		}
	}
	s.notifyRunStarted(execCtx, req, turn)

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

	parentRunID, callID := lineageFromContext(execCtx)
	if req.ParentRunID != "" {
		parentRunID = req.ParentRunID
	}
	if req.CallID != "" {
		callID = req.CallID
	}
	agentRequest := agent.Request{
		Message:     message.NewTextMessage(message.RoleUser, req.Request.Input),
		ParentRunID: parentRunID,
		Attributes:  lineageAttributes(callID),
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

// lineageFromContext derives the run lineage for a delegated subagent
// from the caller's ambient execution context: the caller's run id
// (agent.RunInfo, stamped by the graph engine at the node boundary)
// and the id of the delegate tool call that started this delegation
// (tool.CallIDFromContext, stamped by the tool executor). Either may
// be absent — a tool executed outside an engine run has no ambient
// RunInfo, and direct service calls carry no tool call id — in which
// case the corresponding lineage slot stays empty and the subagent
// runs exactly as it does today.
func lineageFromContext(ctx context.Context) (parentRunID, callID string) {
	if info, ok := agent.RunInfoFromContext(ctx); ok {
		parentRunID = info.RunID
	}
	callID, _ = tool.CallIDFromContext(ctx)
	return parentRunID, callID
}

// lineageAttributes maps a delegate tool call id onto the subagent
// run attribute consumed by the envelope projection. Nil when the
// call id is absent.
func lineageAttributes(callID string) map[string]string {
	if callID == "" {
		return nil
	}
	return map[string]string{telemetry.AttrToolCallID: callID}
}

// injectDelegationLineage adds the service-owned lineage metadata keys
// to an async request's portable metadata so operational views
// surface parent run + call id without per-view plumbing. Returns the
// (possibly newly allocated) metadata map.
func injectDelegationLineage(metadata map[string]string, parentRunID, callID string) map[string]string {
	if parentRunID == "" && callID == "" {
		return metadata
	}
	if metadata == nil {
		metadata = make(map[string]string, 2)
	}
	if parentRunID != "" {
		metadata[ParentRunMetadataKey] = parentRunID
	}
	if callID != "" {
		metadata[CallIDMetadataKey] = callID
	}
	return metadata
}

// streamRefOf returns the escrow ref carried by a StreamRef, or "".
func streamRefOf(ref *StreamRef) string {
	if ref == nil {
		return ""
	}
	return ref.Ref
}

// asyncStreamSpecs captures the caller's inheritable stream sinks for
// an async delegation, downgraded to observers exactly like the sync
// inheritance path. ok=false means there is nothing to stream.
func (s *LocalService) asyncStreamSpecs(ctx context.Context) ([]session.SinkSpec, bool) {
	policy, ok := session.StreamPolicyFromContext(ctx)
	if !ok || !policy.Inheritable || len(policy.Sinks) == 0 {
		return nil, false
	}
	return inheritedSinkSpecs(policy.Sinks), true
}

// exportStreamTarget runs the configured exporter over an inherited
// sink set and returns the chosen target together with the spec it was
// derived from (so the persisted policy snapshot stays aligned with the
// target). Broadcast (bus) targets are preferred: cross-process
// delivery restores exactly one destination, and a bus reaches every
// subscriber. ok=false when no exporter is configured or no sink is
// describable — with an exporter configured that silent degradation is
// logged, because cross-process streaming then silently disappears.
func (s *LocalService) exportStreamTarget(
	ctx context.Context,
	specs []session.SinkSpec,
) (StreamTarget, session.SinkSpec, bool) {
	if s.streamExporter == nil {
		return StreamTarget{}, session.SinkSpec{}, false
	}
	var first StreamTarget
	var firstSpec session.SinkSpec
	var firstOK bool
	for _, spec := range specs {
		target, ok := s.streamExporter(spec)
		if !ok {
			continue
		}
		if target.Kind == StreamTargetKindBus {
			return target, spec, true
		}
		if !firstOK {
			first, firstSpec, firstOK = target, spec, true
		}
	}
	if !firstOK {
		telemetry.Warn(ctx,
			"local delegation: stream exporter matched no sink; "+
				"cross-process streaming will be unavailable",
			otellog.Int("delegation.stream_sinks", len(specs)))
		return StreamTarget{}, session.SinkSpec{}, false
	}
	return first, firstSpec, true
}

// streamPolicySnapshotOf projects one sink spec onto the serializable
// policy snapshot carried by AsyncRequest.Stream. The snapshot is
// derived from the same spec that produced the persisted target, so
// worker-side re-materialization applies matching tuning. Authority and
// ack fields are informational — inherited attachments always downgrade
// to observers.
func streamPolicySnapshotOf(spec session.SinkSpec) StreamPolicySnapshot {
	var snapshot StreamPolicySnapshot
	snapshot.Visibility = spec.Visibility
	snapshot.Authority = spec.Authority
	snapshot.AckMode = spec.AckMode
	snapshot.MaxUnacked = spec.MaxUnacked
	snapshot.QueueSize = spec.QueueSize
	snapshot.DeliveryTimeout = spec.DeliveryTimeout.Milliseconds()
	return snapshot
}

// storeStreamEscrow records the caller's live sink specs for one async
// delegation and returns a release func that must run when the
// delegation reaches its terminal state (or immediately on submit
// failure). Entries are swept by TTL as a backstop.
func (s *LocalService) storeStreamEscrow(ref string, specs []session.SinkSpec) func() {
	s.streamEscrowMu.Lock()
	defer s.streamEscrowMu.Unlock()
	s.sweepStreamEscrowLocked()
	s.streamEscrow[ref] = streamEscrowEntry{
		specs:   append([]session.SinkSpec(nil), specs...),
		expires: time.Now().Add(s.streamEscrowTTL),
	}
	return func() { s.releaseStreamEscrow(ref) }
}

// takeStreamEscrow resolves an escrow ref to the caller's live sink
// specs, refreshing the entry's claim backstop. Missing and expired
// entries report ok=false so the worker degrades to the resolver (or
// no stream at all).
func (s *LocalService) takeStreamEscrow(ref string) ([]session.SinkSpec, bool) {
	if ref == "" {
		return nil, false
	}
	s.streamEscrowMu.Lock()
	defer s.streamEscrowMu.Unlock()
	s.sweepStreamEscrowLocked()
	entry, ok := s.streamEscrow[ref]
	if !ok {
		return nil, false
	}
	entry.expires = time.Now().Add(s.streamEscrowTTL)
	s.streamEscrow[ref] = entry
	return append([]session.SinkSpec(nil), entry.specs...), true
}

// releaseStreamEscrow drops an escrow entry. Idempotent; missing refs
// are no-ops.
func (s *LocalService) releaseStreamEscrow(ref string) {
	if ref == "" {
		return
	}
	s.streamEscrowMu.Lock()
	delete(s.streamEscrow, ref)
	s.streamEscrowMu.Unlock()
}

// sweepStreamEscrowLocked drops expired entries. Callers hold
// streamEscrowMu.
func (s *LocalService) sweepStreamEscrowLocked() {
	now := time.Now()
	for ref, entry := range s.streamEscrow {
		if now.After(entry.expires) {
			delete(s.streamEscrow, ref)
		}
	}
}

// inheritAsyncStreams restores an async work item's stream attachment
// on the worker side. In-process escrow wins; a resolver target is the
// cross-process fallback. The restored attachment is stamped as an
// inheritable stream policy so the session turn (and any nested
// delegation) picks it up through the ordinary sync inheritance path.
func (s *LocalService) inheritAsyncStreams(ctx context.Context, req AsyncRequest) context.Context {
	if req.Stream == nil {
		return ctx
	}
	if specs, ok := s.takeStreamEscrow(req.Stream.Ref); ok {
		return session.WithStreamPolicy(ctx, session.StreamPolicy{
			Sinks:       specs,
			Inheritable: true,
		})
	}
	if s.streamResolver != nil && req.Stream.Target != nil {
		sink, err := s.streamResolver(ctx, *req.Stream.Target)
		if err != nil {
			telemetry.WarnErr(ctx, "local delegation: stream target resolution failed",
				err, otellog.String("delegation.stream_target", req.Stream.Target.ID))
			return ctx
		}
		if isNilInterface(sink) {
			return ctx
		}
		spec := session.SinkSpec{
			ID:              req.Stream.Target.ID,
			Sink:            sink,
			Visibility:      req.Stream.Policy.Visibility,
			Authority:       req.Stream.Policy.Authority,
			AckMode:         req.Stream.Policy.AckMode,
			MaxUnacked:      req.Stream.Policy.MaxUnacked,
			QueueSize:       req.Stream.Policy.QueueSize,
			DeliveryTimeout: time.Duration(req.Stream.Policy.DeliveryTimeout) * time.Millisecond,
		}
		return session.WithStreamPolicy(ctx, session.StreamPolicy{
			Sinks:       inheritedSinkSpecs([]session.SinkSpec{spec}),
			Inheritable: true,
		})
	}
	return ctx
}

// notifyRunStarted records the subagent run id on the async backend
// (when it supports RunIDNotifier) as soon as the turn exists, so
// operational views can correlate delegation ids with runs before the
// terminal response. Best-effort: failures are logged, never fatal.
func (s *LocalService) notifyRunStarted(ctx context.Context, req AsyncRequest, turn *session.Turn) {
	ref := streamRefOf(req.Stream)
	if ref == "" || turn == nil {
		return
	}
	notifier, ok := s.backend.(RunIDNotifier)
	if !ok || isNilInterface(notifier) {
		return
	}
	if err := notifier.NoteRunID(ctx, ref, turn.RunID()); err != nil {
		telemetry.WarnErr(ctx, "local delegation: note run id failed", err,
			otellog.String(telemetry.AttrDelegationTarget, req.Request.Target))
	}
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
// subagent turn: questions to the user are refused (never block),
// non-persistent identities run the turn ephemeral so no session state
// or run checkpoint is ever written, and stream sinks are inherited
// from the caller turn when the caller's stream policy is inheritable.
// Inherited sinks are downgraded to observers (see inheritedSinkSpecs).
// Async worker runs do not inherit here: the caller context does not
// cross the backend queue, and the worker is not a live UI consumer.
func (s *LocalService) delegationStartOptions(ctx context.Context, persistent bool) []session.StartOption {
	options := []session.StartOption{
		session.WithAskUserOverride(refuseSubagentAskUser),
	}
	if !persistent {
		options = append(options, session.WithEphemeral())
	}
	if policy, ok := session.StreamPolicyFromContext(ctx); ok && policy.Inheritable && len(policy.Sinks) > 0 {
		options = append(options, session.WithSinks(inheritedSinkSpecs(policy.Sinks)...))
	}
	return options
}

// inheritedSinkSpecs adapts the caller's sink specs for a subagent turn.
// The subagent turn is a different session whose Turn handle is never
// exposed to the inherited sink, so authoritative and explicit-ack
// obligations cannot be fulfilled there: inherited sinks always attach
// as observers with ack-on-delivery and no unacked window (an unacked
// authoritative attachment would otherwise be detached with a
// BudgetExceeded once its window fills). Visibility is preserved so
// consumers keep the delivery granularity they chose.
func inheritedSinkSpecs(specs []session.SinkSpec) []session.SinkSpec {
	out := make([]session.SinkSpec, 0, len(specs))
	for _, spec := range specs {
		spec.Authority = session.AuthorityObserver
		spec.AckMode = session.AckOnDelivery
		spec.MaxUnacked = 0
		out = append(out, spec)
	}
	return out
}

// sameDelegationRequest reports whether two delegation requests are
// identical for resume purposes: the same user message and the same
// metadata inputs. ContextID and RunID are session plumbing stamped by
// the service and are ignored.
func sameDelegationRequest(a, b agent.Request) bool {
	return a.Message.Role == b.Message.Role &&
		a.Message.Content.Text() == b.Message.Content.Text() &&
		reflect.DeepEqual(a.Inputs, b.Inputs)
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
