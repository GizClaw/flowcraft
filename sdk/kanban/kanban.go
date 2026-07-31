package kanban

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
)

type ctxKey int

const ctxKeyProducerID ctxKey = iota

// WithProducerID marks ctx as belonging to a given producer, typically
// an agent id. [Kanban.Submit] records it on the card so the board
// shows who asked for what.
func WithProducerID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyProducerID, id)
}

// ProducerIDFrom returns the producer id installed by [WithProducerID],
// or "" when absent.
func ProducerIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyProducerID).(string); ok {
		return v
	}
	return ""
}

// Validator vets a task before it is admitted to the board. Returning
// an error rejects the submission and no card is created.
//
// The board has no notion of which agents exist, so this is the seam
// where a host teaches it. Without a Validator, any TargetAgentID is
// accepted and an unroutable card sits pending until something fails
// it.
type Validator func(ctx context.Context, t Task) error

// Kanban is a shared task board: agents submit work, workers claim and
// complete it, and every transition is published on [Kanban.Bus].
//
// Kanban executes nothing. It owns card state and the transitions
// between states; running the work is the executor's job (see the
// package doc). This is what lets one board serve engines, humans, and
// remote services at once — it has no opinion about who does the work.
//
// All methods are safe for concurrent use.
type Kanban struct {
	scopeID    string
	bus        event.Bus
	maxPending int
	cardTTL    time.Duration
	maxCards   int
	validator  Validator

	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.RWMutex
	cards       []*Card
	index       map[string]*Card
	statusCount map[Status]int

	wmu      sync.Mutex
	watchers []*watcher

	metrics   *metrics
	closeOnce sync.Once
}

// Option configures a [Kanban] at construction time.
type Option func(*Kanban)

// WithMaxPending caps how many cards may sit in [StatusPending] at
// once. Further submissions fail with an errdefs.RateLimit error until
// the backlog drains. Zero or negative disables the cap.
//
// This is the board's only load-shedding control, and it is
// deliberately about the queue rather than about concurrency: how many
// cards run in parallel is the executor's decision, not the board's.
func WithMaxPending(n int) Option {
	return func(k *Kanban) { k.maxPending = n }
}

// WithMaxCards caps retained cards. When exceeded, the oldest terminal
// cards are evicted. Non-terminal cards are never evicted.
func WithMaxCards(n int) Option {
	return func(k *Kanban) { k.maxCards = n }
}

// WithCardTTL evicts terminal cards older than d. Non-terminal cards
// are never evicted, however old.
func WithCardTTL(d time.Duration) Option {
	return func(k *Kanban) { k.cardTTL = d }
}

// WithValidator installs a submission gate; see [Validator].
func WithValidator(v Validator) Option {
	return func(k *Kanban) { k.validator = v }
}

// WithBus replaces the in-memory bus with a caller-supplied one, for
// hosts that fan kanban events into an existing transport. The board
// takes ownership: [Kanban.Close] closes it.
func WithBus(b event.Bus) Option {
	return func(k *Kanban) {
		if b != nil {
			k.bus = b
		}
	}
}

// New creates a board. scopeID identifies this board in emitted events
// so consumers can fan in by board without a parallel naming scheme.
func New(scopeID string, opts ...Option) *Kanban {
	ctx, cancel := context.WithCancel(context.Background())
	k := &Kanban{
		scopeID:     scopeID,
		bus:         event.NewMemoryBus(),
		index:       make(map[string]*Card),
		statusCount: make(map[Status]int),
		ctx:         ctx,
		cancel:      cancel,
	}
	for _, opt := range opts {
		opt(k)
	}
	k.metrics = newMetrics(ctx)
	return k
}

// ScopeID returns this board's identifier.
func (k *Kanban) ScopeID() string { return k.scopeID }

// Bus returns the event bus carrying every state transition. It is the
// single source of truth for board activity: the board publishes
// nowhere else.
func (k *Kanban) Bus() event.Bus { return k.bus }

// Context returns a context cancelled when the board closes. Executors
// can derive from it so a board shutdown tears down in-flight work.
func (k *Kanban) Context() context.Context { return k.ctx }

// Close releases the bus and terminates every watcher. Safe to call
// more than once. Close does not wait for executors — the board never
// started them and does not track them; cancel [Kanban.Context] and
// join them on your own side.
func (k *Kanban) Close() error {
	var err error
	k.closeOnce.Do(func() {
		k.cancel()
		k.wmu.Lock()
		watchers := k.watchers
		k.watchers = nil
		k.wmu.Unlock()
		for _, w := range watchers {
			w.shutdown()
		}
		if k.bus != nil {
			err = k.bus.Close()
		}
	})
	return err
}

// Submit admits a task and returns the created card in [StatusPending].
//
// Submit does not execute anything and does not block: it returns as
// soon as the card is on the board. Work begins when an executor
// observes the card, conventionally through [Kanban.Watch], and calls
// [Kanban.Claim].
func (k *Kanban) Submit(ctx context.Context, t Task, opts ...SubmitOption) (*Card, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t.TargetAgentID == "" {
		return nil, errdefs.Validationf("kanban: Task.TargetAgentID is required")
	}
	if t.Timeout < 0 {
		return nil, errdefs.Validationf("kanban: Task.Timeout must not be negative")
	}
	if k.validator != nil {
		if err := k.validator(ctx, t); err != nil {
			return nil, err
		}
	}

	task := t
	task.Inputs = cloneInputs(t.Inputs)

	now := time.Now()
	card := &Card{
		ID:        newCardID(),
		Producer:  ProducerIDFrom(ctx),
		Status:    StatusPending,
		Task:      &task,
		CreatedAt: now,
		UpdatedAt: now,
	}
	for _, opt := range opts {
		opt(card)
	}

	k.mu.Lock()
	if k.isClosed() {
		k.mu.Unlock()
		return nil, errdefs.NotAvailablef("kanban: board is closed")
	}
	if k.maxPending > 0 && k.statusCount[StatusPending] >= k.maxPending {
		k.mu.Unlock()
		return nil, errdefs.RateLimitf(
			"kanban: pending limit reached (%d)", k.maxPending)
	}
	k.cards = append(k.cards, card)
	k.index[card.ID] = card
	k.statusCount[StatusPending]++
	k.evictLocked()
	snap := card.clone()
	k.mu.Unlock()

	k.metrics.cardSubmitted(ctx, task.TargetAgentID, ProducerIDFrom(ctx))
	k.notify(snap)
	k.publish(ctx, snap)
	return snap, nil
}

// Claim moves a pending card to [StatusClaimed] on behalf of consumer.
//
// It is the board's mutual exclusion primitive: when several workers
// watch the same board, exactly one Claim per card returns true, so a
// worker that sees true owns the card and the rest move on. A worker
// MUST reach a terminal state or suspend the cards it claims.
func (k *Kanban) Claim(cardID, consumer string) bool {
	return k.transition(cardID, func(c *Card) bool {
		if c.Status != StatusPending {
			return false
		}
		c.Consumer = consumer
		c.Status = StatusClaimed
		return true
	})
}

// Suspend parks a claimed card in [StatusSuspended], recording an
// opaque resumeRef for whoever continues it.
//
// This is how work that stops without finishing stays visible. An
// engine that interrupts for a human answer, or checkpoints awaiting an
// approval, is neither running nor done: leaving it claimed makes it
// look like a stuck worker, failing it is a lie. Suspend frees the
// worker while keeping the card alive, and resumeRef — typically a
// checkpoint id — is handed back on [Kanban.Resume]. The board stores
// it verbatim and never interprets it.
func (k *Kanban) Suspend(cardID, resumeRef string) bool {
	return k.transition(cardID, func(c *Card) bool {
		if c.Status != StatusClaimed {
			return false
		}
		c.Status = StatusSuspended
		c.ResumeRef = resumeRef
		c.Consumer = ""
		return true
	})
}

// Resume returns a suspended card to [StatusPending] so it is picked up
// again, and reports the resume reference recorded by [Kanban.Suspend].
// The reference is left on the card: the executor needs it to rebuild
// state, and a later reader still wants to know the card was resumed.
func (k *Kanban) Resume(cardID string) (string, bool) {
	var ref string
	ok := k.transition(cardID, func(c *Card) bool {
		if c.Status != StatusSuspended {
			return false
		}
		ref = c.ResumeRef
		c.Status = StatusPending
		return true
	})
	return ref, ok
}

// Done completes a claimed card with its output.
func (k *Kanban) Done(cardID string, r Result) bool {
	return k.transition(cardID, func(c *Card) bool {
		if c.Status != StatusClaimed {
			return false
		}
		c.Status = StatusDone
		result := r
		c.Result = &result
		return true
	})
}

// Fail terminates a claimed card with an error message.
func (k *Kanban) Fail(cardID, errMsg string) bool {
	return k.transition(cardID, func(c *Card) bool {
		if c.Status != StatusClaimed {
			return false
		}
		c.Status = StatusFailed
		c.Result = &Result{Error: errMsg}
		return true
	})
}

// Cancel abandons any non-terminal card, recording reason.
//
// Cancel is separate from Fail so that orderly shutdown does not
// pollute failure metrics: a card nobody will ever run is cancelled,
// a card that ran and errored is failed.
func (k *Kanban) Cancel(cardID, reason string) bool {
	return k.transition(cardID, func(c *Card) bool {
		if c.Status.IsTerminal() {
			return false
		}
		c.Status = StatusCancelled
		c.Result = &Result{Error: reason}
		return true
	})
}

// transition applies mutate under the board lock, then publishes the
// resulting snapshot. mutate reports whether the transition applied.
func (k *Kanban) transition(cardID string, mutate func(*Card) bool) bool {
	k.mu.Lock()
	card, ok := k.index[cardID]
	if !ok {
		k.mu.Unlock()
		return false
	}
	from := card.Status
	if !mutate(card) {
		k.mu.Unlock()
		return false
	}
	card.UpdatedAt = time.Now()
	k.statusCount[from]--
	k.statusCount[card.Status]++
	snap := card.clone()
	k.mu.Unlock()

	k.metrics.cardTransitioned(k.ctx, snap)
	k.notify(snap)
	k.publish(k.ctx, snap)
	return true
}

// Card returns a copy of one card.
func (k *Kanban) Card(cardID string) (*Card, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	c, ok := k.index[cardID]
	if !ok {
		return nil, false
	}
	return c.clone(), true
}

// Query returns copies of every card matching f, in submission order.
func (k *Kanban) Query(f Filter) []*Card {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]*Card, 0, len(k.cards))
	for _, c := range k.cards {
		if f.matches(c) {
			out = append(out, c.clone())
		}
	}
	return out
}

// CountByStatus reports how many retained cards are in status.
func (k *Kanban) CountByStatus(status Status) int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.statusCount[status]
}

// Len reports how many cards the board retains.
func (k *Kanban) Len() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.cards)
}

// evictLocked drops the oldest terminal cards once the TTL or the card
// cap is exceeded. Non-terminal cards are never evicted: a pending or
// suspended card is outstanding work, and silently dropping it would
// lose it. Must hold k.mu.
func (k *Kanban) evictLocked() {
	if k.maxCards <= 0 && k.cardTTL <= 0 {
		return
	}
	if k.cardTTL > 0 {
		now := time.Now()
		kept := k.cards[:0]
		for _, c := range k.cards {
			if c.Status.IsTerminal() && now.Sub(c.UpdatedAt) > k.cardTTL {
				delete(k.index, c.ID)
				k.statusCount[c.Status]--
				continue
			}
			kept = append(kept, c)
		}
		k.cards = kept
	}
	if k.maxCards > 0 && len(k.cards) > k.maxCards {
		excess := len(k.cards) - k.maxCards
		kept := k.cards[:0]
		for _, c := range k.cards {
			if excess > 0 && c.Status.IsTerminal() {
				delete(k.index, c.ID)
				k.statusCount[c.Status]--
				excess--
				continue
			}
			kept = append(kept, c)
		}
		k.cards = kept
	}
}

func (k *Kanban) isClosed() bool {
	select {
	case <-k.ctx.Done():
		return true
	default:
		return false
	}
}

func newCardID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("kanban: failed to generate card id: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
