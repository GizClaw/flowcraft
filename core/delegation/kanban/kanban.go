package kanban

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/delegation"
	sdkdelegation "github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
)

const (
	workSourceConsumer          = "delegation-worker"
	defaultIdempotencyRetention = time.Hour
	maxJanitorInterval          = time.Minute
)

// Validator vets a typed asynchronous delegation before it is admitted.
type Validator func(context.Context, delegation.AsyncRequest) error

// Board is an in-memory delegation AsyncBackend and WorkSource.
//
// Its card, query, transition, watch, event, and metric APIs are operational
// views of that backend. AsyncBackend callers receive only delegation ids and
// delegation responses. Operational cards and terminal idempotency records
// have independent retention: terminal cards may be evicted from the board
// while their minimal replay record remains queryable.
type Board struct {
	scopeID              string
	bus                  event.Bus
	ownsBus              bool
	maxPending           int
	cardTTL              time.Duration
	maxCards             int
	idempotencyRetention time.Duration
	validator            Validator

	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.RWMutex
	closed      bool
	cards       []*Card
	index       map[string]*Card
	idempotency map[string]*retainedOperation
	tombstones  map[string]*retainedOperation
	statusCount map[Status]int
	workReady   chan struct{}
	leases      map[string]lease

	wmu      sync.Mutex
	watchers []*watcher

	metrics   *metrics
	janitor   sync.WaitGroup
	closeOnce sync.Once
}

type retainedOperation struct {
	id          string
	key         string
	fingerprint [sha256.Size]byte
	response    sdkdelegation.Response
	expires     time.Time
}

type lease struct {
	token  string
	cancel context.CancelFunc
}

// Option configures a Board.
type Option func(*Board)

// WithMaxPending caps delegations waiting to be claimed.
func WithMaxPending(n int) Option {
	return func(board *Board) { board.maxPending = n }
}

// WithMaxCards caps retained cards by evicting the oldest terminal cards.
func WithMaxCards(n int) Option {
	return func(board *Board) { board.maxCards = n }
}

// WithCardTTL evicts terminal cards older than d during admission.
func WithCardTTL(d time.Duration) Option {
	return func(board *Board) { board.cardTTL = d }
}

// WithIdempotencyRetention sets how long a terminal operation remains
// replayable and queryable after completion. The duration must be positive.
func WithIdempotencyRetention(d time.Duration) Option {
	return func(board *Board) { board.idempotencyRetention = d }
}

// WithValidator installs an asynchronous delegation admission gate.
func WithValidator(validator Validator) Option {
	return func(board *Board) { board.validator = validator }
}

// WithBus publishes backend events to bus without transferring ownership.
func WithBus(bus event.Bus) Option {
	return func(board *Board) {
		if bus != nil {
			board.bus = bus
			board.ownsBus = false
		}
	}
}

// New constructs an in-memory delegation backend.
func New(scopeID string, options ...Option) *Board {
	ctx, cancel := context.WithCancel(context.Background())
	board := &Board{
		scopeID:              scopeID,
		bus:                  event.NewMemoryBus(),
		ownsBus:              true,
		ctx:                  ctx,
		cancel:               cancel,
		index:                make(map[string]*Card),
		idempotency:          make(map[string]*retainedOperation),
		tombstones:           make(map[string]*retainedOperation),
		statusCount:          make(map[Status]int),
		workReady:            make(chan struct{}),
		leases:               make(map[string]lease),
		idempotencyRetention: defaultIdempotencyRetention,
	}
	for _, option := range options {
		if option != nil {
			option(board)
		}
	}
	if board.idempotencyRetention <= 0 {
		panic("delegation kanban: idempotency retention must be positive")
	}
	board.metrics = newMetrics(ctx)
	board.janitor.Add(1)
	go board.runJanitor()
	return board
}

// ScopeID returns the backend instance identifier used by events.
func (b *Board) ScopeID() string { return b.scopeID }

// Bus returns the event bus carrying delegation card transitions.
func (b *Board) Bus() event.Bus { return b.bus }

// Context is canceled when the backend closes.
func (b *Board) Context() context.Context { return b.ctx }

// IdempotencyRetention reports the guaranteed terminal replay window.
func (b *Board) IdempotencyRetention() time.Duration { return b.idempotencyRetention }

// Close wakes blocked claims, stops watchers, and closes an owned bus.
func (b *Board) Close() error {
	if b == nil {
		return nil
	}
	var closeErr error
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		leaseCancels := make([]context.CancelFunc, 0, len(b.leases))
		for id, current := range b.leases {
			leaseCancels = append(leaseCancels, current.cancel)
			delete(b.leases, id)
		}
		b.mu.Unlock()
		for _, cancel := range leaseCancels {
			cancel()
		}
		b.cancel()
		b.janitor.Wait()

		b.wmu.Lock()
		watchers := b.watchers
		b.watchers = nil
		b.wmu.Unlock()
		for _, watcher := range watchers {
			watcher.shutdown()
		}
		if b.bus != nil && b.ownsBus {
			closeErr = b.bus.Close()
		}
	})
	return closeErr
}

// Submit implements delegation.AsyncBackend. It admits an asynchronous
// delegation and returns only its backend id.
func (b *Board) Submit(ctx context.Context, request delegation.AsyncRequest) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.Request.Validate(); err != nil {
		return "", err
	}
	if request.Request.Mode != sdkdelegation.ModeAsync {
		return "", errdefs.Validationf(
			"delegation kanban: request mode must be %q", sdkdelegation.ModeAsync)
	}
	request = cloneAsyncRequest(request)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return "", errdefs.NotAvailablef("delegation kanban: backend is closed")
	}
	b.evictLocked()
	if id, found, err := b.idempotentSubmissionLocked(request); found || err != nil {
		b.mu.Unlock()
		return id, err
	}
	b.mu.Unlock()

	if b.validator != nil {
		if err := b.validator(ctx, cloneAsyncRequest(request)); err != nil {
			return "", err
		}
	}

	now := time.Now()
	card := &Card{
		ID:        newCardID(),
		Producer:  request.Caller,
		Status:    StatusPending,
		Task:      &Task{Request: request},
		CreatedAt: now,
		UpdatedAt: now,
		Meta:      cloneMetadata(request.Request.Metadata),
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return "", errdefs.NotAvailablef("delegation kanban: backend is closed")
	}
	b.evictLocked()
	if id, found, err := b.idempotentSubmissionLocked(request); found || err != nil {
		b.mu.Unlock()
		return id, err
	}
	if b.maxPending > 0 && b.statusCount[StatusPending] >= b.maxPending {
		b.mu.Unlock()
		return "", errdefs.RateLimitf(
			"delegation kanban: pending limit reached (%d)", b.maxPending)
	}
	b.cards = append(b.cards, card)
	b.index[card.ID] = card
	if key := request.Request.IdempotencyKey; key != "" {
		b.idempotency[key] = &retainedOperation{
			id:          card.ID,
			key:         key,
			fingerprint: asyncRequestFingerprint(request),
		}
	}
	b.statusCount[StatusPending]++
	b.evictLocked()
	b.signalWorkLocked()
	snapshot := card.clone()
	b.mu.Unlock()

	b.metrics.cardSubmitted(ctx, snapshot)
	b.notify(snapshot)
	b.publish(ctx, snapshot)
	return card.ID, nil
}

func (b *Board) idempotentSubmissionLocked(
	request delegation.AsyncRequest,
) (string, bool, error) {
	key := request.Request.IdempotencyKey
	if key == "" {
		return "", false, nil
	}
	retained, ok := b.idempotency[key]
	if !ok {
		return "", false, nil
	}
	if !retained.expires.IsZero() && !time.Now().Before(retained.expires) {
		delete(b.idempotency, key)
		delete(b.tombstones, retained.id)
		return "", false, nil
	}
	if retained.fingerprint != asyncRequestFingerprint(request) {
		return "", false, errdefs.Conflictf(
			"delegation kanban: idempotency key %q was already used for a different request",
			key,
		)
	}
	return retained.id, true, nil
}

// Status implements delegation.AsyncBackend.
func (b *Board) Status(ctx context.Context, id string) (sdkdelegation.Response, error) {
	if id == "" {
		return sdkdelegation.Response{}, errdefs.Validationf(
			"delegation kanban: delegation id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return sdkdelegation.Response{}, ctx.Err()
	default:
	}
	b.mu.RLock()
	card, ok := b.index[id]
	if ok {
		response := responseForCard(card)
		b.mu.RUnlock()
		return response, nil
	}
	retained, ok := b.tombstones[id]
	if ok && time.Now().Before(retained.expires) {
		response := cloneResponse(retained.response)
		b.mu.RUnlock()
		return response, nil
	}
	b.mu.RUnlock()
	return sdkdelegation.Response{}, sdkdelegation.RequestNotFound(id)
}

// Claim implements delegation.WorkSource. It blocks until pending work is
// available, ctx is canceled, or the backend closes.
func (b *Board) Claim(ctx context.Context) (delegation.Work, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return delegation.Work{}, errdefs.NotAvailablef(
				"delegation kanban: backend is closed")
		}
		for _, card := range b.cards {
			if card.Status != StatusPending {
				continue
			}
			snapshot := b.claimLocked(card, workSourceConsumer)
			leaseCtx, cancelLease := context.WithCancel(b.ctx)
			leaseToken := newLeaseToken()
			b.leases[card.ID] = lease{token: leaseToken, cancel: cancelLease}
			b.mu.Unlock()
			b.afterTransition(snapshot)
			return delegation.Work{
				ID:         snapshot.ID,
				LeaseToken: leaseToken,
				Request:    cloneAsyncRequest(snapshot.Task.Request),
				Context:    leaseCtx,
			}, nil
		}
		ready := b.workReady
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return delegation.Work{}, ctx.Err()
		case <-b.ctx.Done():
			return delegation.Work{}, errdefs.NotAvailablef(
				"delegation kanban: backend is closed")
		case <-ready:
		}
	}
}

// Complete implements delegation.WorkSource. A response for a stale lease is
// ignored. The current lease's response must be terminal, and its status
// determines the card's terminal state.
func (b *Board) Complete(
	ctx context.Context,
	id string,
	leaseToken string,
	response sdkdelegation.Response,
) error {
	if id == "" {
		return errdefs.Validationf("delegation kanban: delegation id is required")
	}
	if leaseToken == "" {
		return errdefs.Validationf("delegation kanban: lease token is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	b.mu.Lock()
	card, ok := b.index[id]
	if !ok {
		b.mu.Unlock()
		return nil
	}
	current, leased := b.leases[id]
	if !leased || current.token != leaseToken {
		b.mu.Unlock()
		return nil
	}
	if card.Status != StatusClaimed {
		status := card.Status
		b.mu.Unlock()
		return errdefs.Conflictf(
			"delegation kanban: cannot complete %q card", status)
	}
	response.ID = id
	response = cloneResponse(response)
	if err := response.Validate(); err != nil {
		b.mu.Unlock()
		return err
	}
	if !response.Status.Terminal() {
		b.mu.Unlock()
		return errdefs.Validationf(
			"delegation kanban: completion status %q is not terminal", response.Status)
	}
	from := card.Status
	switch response.Status {
	case sdkdelegation.StatusSucceeded:
		card.Status = StatusDone
	case sdkdelegation.StatusFailed:
		card.Status = StatusFailed
	case sdkdelegation.StatusCanceled:
		card.Status = StatusCancelled
	}
	card.Result = &Result{Response: response}
	snapshot := b.finishTransitionLocked(card, from)
	cancelLease := b.takeLeaseLocked(id)
	b.mu.Unlock()
	if cancelLease != nil {
		cancelLease()
	}
	b.afterTransition(snapshot)
	return nil
}

// ClaimCard applies an explicit operational claim to one pending card.
func (b *Board) ClaimCard(id, consumer string) bool {
	b.mu.Lock()
	card, ok := b.index[id]
	if !ok || card.Status != StatusPending {
		b.mu.Unlock()
		return false
	}
	snapshot := b.claimLocked(card, consumer)
	b.mu.Unlock()
	b.afterTransition(snapshot)
	return true
}

// Suspend parks a claimed delegation until Resume is called.
func (b *Board) Suspend(id, resumeRef string) bool {
	return b.transition(id, func(card *Card) bool {
		if card.Status != StatusClaimed {
			return false
		}
		card.Status = StatusSuspended
		card.ResumeRef = resumeRef
		card.Consumer = ""
		return true
	})
}

// Resume returns a suspended delegation to pending work.
func (b *Board) Resume(id string) (string, bool) {
	var resumeRef string
	ok := b.transition(id, func(card *Card) bool {
		if card.Status != StatusSuspended {
			return false
		}
		resumeRef = card.ResumeRef
		card.Status = StatusPending
		return true
	})
	return resumeRef, ok
}

// Cancel terminates any non-terminal delegation.
func (b *Board) Cancel(id, reason string) bool {
	response := sdkdelegation.Response{
		ID:     id,
		Status: sdkdelegation.StatusCanceled,
		Error:  reason,
	}
	return b.transition(id, func(card *Card) bool {
		if card.Status.IsTerminal() {
			return false
		}
		card.Status = StatusCancelled
		card.Result = &Result{Response: response}
		return true
	})
}

// Card returns an operational copy of one delegation card.
func (b *Board) Card(id string) (*Card, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	card, ok := b.index[id]
	if !ok {
		return nil, false
	}
	return card.clone(), true
}

// Query returns operational card copies in admission order.
func (b *Board) Query(filter Filter) []*Card {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]*Card, 0, len(b.cards))
	for _, card := range b.cards {
		if filter.matches(card) {
			out = append(out, card.clone())
		}
	}
	return out
}

// CountByStatus reports retained cards in an internal state.
func (b *Board) CountByStatus(status Status) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.statusCount[status]
}

// Len reports the number of retained delegation cards.
func (b *Board) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.cards)
}

func (b *Board) transition(id string, mutate func(*Card) bool) bool {
	b.mu.Lock()
	card, ok := b.index[id]
	if !ok {
		b.mu.Unlock()
		return false
	}
	from := card.Status
	if !mutate(card) {
		b.mu.Unlock()
		return false
	}
	snapshot := b.finishTransitionLocked(card, from)
	var cancelLease context.CancelFunc
	if from == StatusClaimed && card.Status != StatusClaimed {
		cancelLease = b.takeLeaseLocked(id)
	}
	if card.Status == StatusPending {
		b.signalWorkLocked()
	}
	b.mu.Unlock()
	if cancelLease != nil {
		cancelLease()
	}
	b.afterTransition(snapshot)
	return true
}

func (b *Board) claimLocked(card *Card, consumer string) *Card {
	from := card.Status
	card.Status = StatusClaimed
	card.Consumer = consumer
	return b.finishTransitionLocked(card, from)
}

func (b *Board) finishTransitionLocked(card *Card, from Status) *Card {
	card.UpdatedAt = time.Now()
	b.statusCount[from]--
	b.statusCount[card.Status]++
	if card.Status.IsTerminal() && card.Result != nil {
		b.retainTerminalLocked(card)
	}
	return card.clone()
}

func (b *Board) signalWorkLocked() {
	close(b.workReady)
	b.workReady = make(chan struct{})
}

func (b *Board) takeLeaseLocked(id string) context.CancelFunc {
	current := b.leases[id]
	delete(b.leases, id)
	return current.cancel
}

func (b *Board) afterTransition(snapshot *Card) {
	b.metrics.cardTransitioned(b.ctx, snapshot)
	b.notify(snapshot)
	b.publish(b.ctx, snapshot)
}

func (b *Board) evictLocked() {
	b.expireRetainedLocked(time.Now())
	if b.cardTTL > 0 {
		now := time.Now()
		kept := b.cards[:0]
		for _, card := range b.cards {
			if card.Status.IsTerminal() && now.Sub(card.UpdatedAt) > b.cardTTL {
				b.removeCardLocked(card)
				b.statusCount[card.Status]--
				continue
			}
			kept = append(kept, card)
		}
		b.cards = kept
	}
	if b.maxCards <= 0 || len(b.cards) <= b.maxCards {
		return
	}
	excess := len(b.cards) - b.maxCards
	kept := b.cards[:0]
	for _, card := range b.cards {
		if excess > 0 && card.Status.IsTerminal() {
			b.removeCardLocked(card)
			b.statusCount[card.Status]--
			excess--
			continue
		}
		kept = append(kept, card)
	}
	b.cards = kept
}

func (b *Board) removeCardLocked(card *Card) {
	delete(b.index, card.ID)
	if !card.Status.IsTerminal() || card.Result == nil {
		return
	}
	b.retainTerminalLocked(card)
}

func (b *Board) retainTerminalLocked(card *Card) {
	if card == nil || card.Task == nil || card.Result == nil {
		return
	}
	key := card.Task.Request.Request.IdempotencyKey
	expires := card.UpdatedAt.Add(b.idempotencyRetention)
	if !time.Now().Before(expires) {
		return
	}
	retained := b.idempotency[key]
	if key == "" || retained == nil || retained.id != card.ID {
		retained = &retainedOperation{
			id:          card.ID,
			key:         key,
			fingerprint: asyncRequestFingerprint(card.Task.Request),
		}
		if key != "" && b.idempotency[key] == nil {
			b.idempotency[key] = retained
		}
	}
	retained.response = cloneResponse(card.Result.Response)
	retained.response.ID = card.ID
	retained.expires = expires
	b.tombstones[card.ID] = retained
}

func (b *Board) expireRetainedLocked(now time.Time) {
	for id, retained := range b.tombstones {
		if now.Before(retained.expires) {
			continue
		}
		delete(b.tombstones, id)
		key := retained.key
		if key != "" && b.idempotency[key] == retained {
			delete(b.idempotency, key)
		}
	}
}

func (b *Board) runJanitor() {
	defer b.janitor.Done()
	interval := b.idempotencyRetention / 2
	if b.cardTTL > 0 && b.cardTTL/2 < interval {
		interval = b.cardTTL / 2
	}
	if interval > maxJanitorInterval {
		interval = maxJanitorInterval
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			b.mu.Lock()
			if !b.closed {
				b.evictLocked()
			}
			b.mu.Unlock()
		}
	}
}

func asyncRequestFingerprint(request delegation.AsyncRequest) [sha256.Size]byte {
	encoded, err := json.Marshal(request)
	if err != nil {
		panic("delegation kanban: fingerprint request: " + err.Error())
	}
	return sha256.Sum256(encoded)
}

func responseForCard(card *Card) sdkdelegation.Response {
	if card.Result != nil {
		response := cloneResponse(card.Result.Response)
		response.ID = card.ID
		return response
	}
	status := sdkdelegation.StatusAccepted
	if card.Status == StatusClaimed {
		status = sdkdelegation.StatusRunning
	}
	return sdkdelegation.Response{ID: card.ID, Status: status}
}

func newCardID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic("delegation kanban: generate card id: " + err.Error())
	}
	return hex.EncodeToString(bytes[:])
}

func newLeaseToken() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic("delegation kanban: generate lease token: " + err.Error())
	}
	return hex.EncodeToString(bytes[:])
}

var (
	_ delegation.AsyncBackend                 = (*Board)(nil)
	_ delegation.IdempotencyRetentionProvider = (*Board)(nil)
	_ delegation.WorkSource                   = (*Board)(nil)
)
