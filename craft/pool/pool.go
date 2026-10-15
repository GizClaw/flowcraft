package pool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// Key is the pool's unit of identity: a kind and an id inside it. The
// kind is the application's namespace — "workspace", "app", whatever
// the application pools by — and it is part of the identity on purpose:
// the same id in two kinds is two keys, so a directory named like an
// application can never resolve to the application's member.
//
// Neither part is interpreted here. A key with an empty or blank part
// is not a key: the two calls that must answer with a member (Ensure,
// Acquire) refuse it with ErrNoKey rather than serve whatever a blank
// id cleans to, and the calls that retire or read answer with nothing.
//
// Keys compare with ==, which is what the pool itself does; an
// application that takes paths as ids cleans them before building the
// key, so two spellings of one directory are one key.
type Key struct {
	Kind string
	ID   string
}

// Valid reports whether the key names something: both parts have to
// carry a non-blank character. A blank part is what an application
// passes when no workspace is selected and no application chosen;
// serving it would mean serving somebody else's directory.
func (k Key) Valid() bool {
	return strings.TrimSpace(k.Kind) != "" && strings.TrimSpace(k.ID) != ""
}

// String renders the key for logs: "kind=id", or "key=<none>" for an
// incomplete one.
func (k Key) String() string {
	if !k.Valid() {
		return "key=<none>"
	}
	return k.Kind + "=" + k.ID
}

// Errors the pool returns, or expects to see.
var (
	// ErrNoKey reports an Ensure or Acquire that named no key.
	ErrNoKey = errors.New("pool: no key to serve")
	// ErrNoMember reports an assembly that returned no member and no
	// error. Returning (nil, nil) is a bug in the Open hook; reporting
	// it beats publishing a member the pool cannot hand out.
	ErrNoMember = errors.New("pool: assembly produced no member")
	// ErrNotReady is what Do reports when its window closed without a
	// usable member. It is always retryable: it stands for a member
	// that is still coming up.
	ErrNotReady = errors.New("pool: member is not ready")
)

// Spec is the application's half of the pool: how a member is built and
// torn down, what the pool may ask about it, and what it says when a
// replacement lands.
//
// The hooks are called without the pool lock held. Two of them —
// WaitClosed and Replaced — also run on the pool's own goroutines, so
// every hook must be safe for concurrent use, and none of them may call
// back into the pool synchronously from a hand-out path (an Installed
// hook calling Ensure for the member it was just handed deadlocks).
type Spec[T any] struct {
	// Open assembles the member for k. It runs outside the pool lock;
	// whatever the assembly needs beyond the key — a reason tagged on
	// ctx, a per-call backend — travels on ctx, which is the caller's
	// (Ensure, Acquire, or the pool's own deferred rebuild).
	Open func(ctx context.Context, k Key) (*T, error)

	// Close begins the member's teardown: stop accepting work, drain
	// what is in flight, release what it holds. The pool calls it at
	// most once per member, never under its lock. Close returning says
	// teardown started, not that it finished — WaitClosed answers that.
	Close func(v *T)

	// Busy reports whether the member still has work in flight. A
	// member invalidated while busy stays pooled and keeps serving that
	// work on the old generation; an idle one closes right away, unless
	// a caller is still holding it (see Acquire). Nil means never busy.
	Busy func(v *T) bool

	// WaitClosed blocks until the member's teardown finished, or ctx is
	// done (returning ctx.Err()). The pool calls it right after Close,
	// so it never has to answer for a member whose teardown has not
	// begun. Nil means Close tears the member down synchronously: its
	// return is the application's word that the member is down.
	//
	// Teardown is expected to finish on its own: the pool waits for a
	// retired member without a deadline of its own, as do the callers
	// it makes wait. A member that never finishes tearing down keeps
	// its key from being served again.
	WaitClosed func(ctx context.Context, v *T) error

	// Installed is applied once per member, on every path that hands
	// one out (Ensure's pooled branch, Acquire's return). It is where
	// an application wires what has to be wired exactly once — event
	// sinks, observers — no matter how often a member is handed out and
	// whichever path did it. Nil means nothing to wire.
	//
	// A member the pool has already forgotten is skipped rather than
	// wired late: the apply-once marker rides the pool entry, so it
	// goes away with the member.
	Installed func(k Key, v *T)

	// Wanted reports whether k still wants a member at all. It is asked
	// once, after the retiring member's teardown and before a deferred
	// replacement is assembled: an application whose user has left the
	// key meanwhile keeps the pool from building a member nobody is
	// going to look at. Nil means every key is wanted.
	Wanted func(k Key) bool

	// Replaced is notified after a deferred replacement was assembled
	// and pooled, so the application can refresh whatever it renders
	// from that member. It runs on the pool's watcher goroutine and
	// must not block. Nil means no notification.
	Replaced func(k Key, v *T)

	// Retryable classifies an error as one that waiting for the key's
	// replacement member can fix — the application's lifecycle guards,
	// "this generation is closing", "the store is not ready yet". Do
	// retries those and reports everything else as it is. Nil means
	// nothing is retryable.
	Retryable func(err error) bool

	// RetryWindow bounds how long Do waits for a member, across all its
	// attempts. Zero means DefaultRetryWindow.
	RetryWindow time.Duration

	// RetryAttempts caps how many times Do asks for a member. Zero
	// means DefaultRetryAttempts. The first attempt always runs.
	RetryAttempts int
}

// Pool keeps one member per key. Members retire on invalidation and are
// replaced on the next Ensure or Acquire; the pool owns that
// bookkeeping and never holds its lock across an assembly or a
// teardown.
type Pool[T any] struct {
	spec Spec[T]

	mu sync.Mutex
	// pooled holds the members the pool serves, by key.
	pooled map[Key]*entry[T]
	// stale holds the keys whose current generation was invalidated and
	// whose replacement has not landed yet. It deliberately outlives
	// the member's retirement: a caller that was handed the retired
	// member still gets "yes, stale" from Stale, which is its cue to
	// ask for a replacement.
	stale map[Key]struct{}
	// retiring holds members that left the pool and are draining their
	// last work. Acquire waits them out instead of assembling a second
	// member for the key: two generations serving one key at once is
	// the failure mode this map exists for.
	retiring map[Key]*entry[T]
	// assembling holds the in-flight assembly per key so a burst of
	// callers shares one build instead of racing — and so one broken
	// assembly is reported once rather than retried per waiter (see
	// assemblyCall).
	assembling map[Key]*assemblyCall
	// armed holds the keys whose deferred replacement is scheduled (see
	// ScheduleReplacement): one per key, so a storm of invalidations
	// inside one drain asks for one replacement.
	armed map[Key]struct{}
	// assemblies counts each key's assemblies in this pool. A rebuild
	// storm is visible as this number climbing: a key is supposed to
	// assemble once per change that needs a new generation, not once
	// per call.
	assemblies map[Key]int
}

// entry is the pool's bookkeeping for one member.
type entry[T any] struct {
	value *T
	refs  int
	// installed records that the configure-once Installed hook already
	// ran for this member. The marker rides the entry on purpose: it
	// goes away together with the member, so a retired member is never
	// wired late and stays collectable.
	installed bool
	// closed fires once the member's teardown finished: Close returned,
	// and WaitClosed — where the application has one — completed after
	// it. Every wait in the pool is on this signal rather than on the
	// application's hook, which is what makes waiting for a member
	// independent of where its Close happens to run: a member whose
	// teardown is in flight, one whose teardown has not begun yet and
	// one already down are all waited for the same way.
	closed chan struct{}
}

// newEntry starts the bookkeeping for a member the pool is about to
// publish. The teardown signal comes with it: a member is handed out,
// and waited for, with the same entry.
func newEntry[T any](v *T) *entry[T] {
	return &entry[T]{value: v, closed: make(chan struct{})}
}

// assemblyCall is one in-flight assembly shared by every Acquire caller
// that asked for the same key while it ran.
type assemblyCall struct {
	done chan struct{}
	err  error
}

// New returns a pool for the given spec. Open and Close are required:
// the pool cannot assemble or retire anything without them.
func New[T any](spec Spec[T]) (*Pool[T], error) {
	if spec.Open == nil {
		return nil, errdefs.Validationf("craft pool: Open is required")
	}
	if spec.Close == nil {
		return nil, errdefs.Validationf("craft pool: Close is required")
	}
	if spec.RetryWindow < 0 {
		return nil, errdefs.Validationf(
			"craft pool: RetryWindow must not be negative")
	}
	if spec.RetryAttempts < 0 {
		return nil, errdefs.Validationf(
			"craft pool: RetryAttempts must not be negative")
	}
	return &Pool[T]{
		spec:   spec,
		pooled: make(map[Key]*entry[T]),
		stale:  make(map[Key]struct{}),
	}, nil
}

// Ensure returns the member that serves k, assembling one when the key
// has none to serve work: the pooled member when there is one — a
// member invalidated but still serving its last work counts — and
// otherwise a fresh assembly, once any member retiring for k has
// finished teardown. It never assembles a second member while one is
// draining, and never hands out another key's.
//
// There is no closing generation to filter here: a member whose
// teardown has begun is not pooled any more, and the paths that would
// hand one out are the ones that wait for it instead.
//
// It takes no reference: Ensure is the read path, called once per unit
// of work, and the member it returns is the pool's to keep or retire —
// a borrow never holds a retirement back, so a member this call
// assembled can be retired by the key's next invalidation while the
// caller is still using it. A caller that must hold the member against
// retirement uses Acquire and Release instead.
//
// A member it returns has Installed applied, whether it came out of the
// pool or from an assembly this call started. A nil ctx is treated as
// context.Background().
func (p *Pool[T]) Ensure(ctx context.Context, k Key) (*T, error) {
	if !k.Valid() {
		return nil, ErrNoKey
	}
	p.mu.Lock()
	e := p.pooled[k]
	p.mu.Unlock()
	if e != nil {
		// This branch hands out a member the assembler may not have
		// reached yet (Installed runs after publishing) and one that was
		// pooled before the hook was installed; the wiring is applied
		// here too, so no hand-out path leaves a member that can serve
		// work unwired.
		p.install(k, e.value)
		return e.value, nil
	}
	v, err := p.serve(ctx, k, false)
	if err != nil {
		return nil, err
	}
	p.install(k, v)
	return v, nil
}

// Acquire returns the member that serves k, assembling one if needed,
// and checks it out: the caller owes exactly one Release. It waits out
// a retiring member's teardown rather than hand out a member on its way
// out, and concurrent callers for one key share a single assembly —
// whoever gets there first builds, the rest wait and reuse the result,
// error included. Every member it returns has Installed applied first.
//
// A nil ctx is treated as context.Background().
func (p *Pool[T]) Acquire(ctx context.Context, k Key) (*T, error) {
	v, err := p.serve(ctx, k, true)
	if err != nil {
		return nil, err
	}
	p.install(k, v)
	return v, nil
}

// serve is the pool half both entrance points share: it resolves or
// assembles the member for one key and leaves the configure-once wiring
// to the caller. It is also the one place a call that named no key is
// refused, so an empty kind or id cannot assemble a member for whatever
// it cleans to.
//
// checkout says whether the caller takes a reference to the member:
// Acquire does, and owes a Release, while Ensure borrows without one.
// The distinction is the whole of the reference protocol: a member is
// held against its invalidation by the application's report of work in
// flight and by the pool's own count of the callers still using it, and
// which half a hand-out owes is decided here, at the one place a member
// changes hands.
func (p *Pool[T]) serve(ctx context.Context, k Key, checkout bool) (*T, error) {
	if !k.Valid() {
		return nil, ErrNoKey
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		p.mu.Lock()
		if e := p.pooled[k]; e != nil {
			if checkout {
				e.refs++
			}
			v := e.value
			p.mu.Unlock()
			return v, nil
		}
		if e := p.retiring[k]; e != nil {
			p.mu.Unlock()
			// The member on its way out is waited for, never handed
			// out: it belongs to the previous generation, and the
			// signal this waits on is set once its teardown finished.
			if err := p.waitMember(ctx, e); err != nil {
				return nil, err
			}
			continue
		}
		call := p.assembling[k]
		if call == nil {
			call = &assemblyCall{done: make(chan struct{})}
			if p.assembling == nil {
				p.assembling = make(map[Key]*assemblyCall)
			}
			p.assembling[k] = call
			p.mu.Unlock()
			return p.assembleShared(ctx, k, call, checkout)
		}
		p.mu.Unlock()
		select {
		case <-call.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if call.err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// A leader cancelled by its own caller is not this caller's
			// failure, and ours is still live: the next pass assembles.
			// Any other error is shared rather than retried once per
			// waiter.
			if !errors.Is(call.err, context.Canceled) &&
				!errors.Is(call.err, context.DeadlineExceeded) {
				return nil, call.err
			}
		}
	}
}

// Current returns the member that serves k right now: the pooled one
// when one is installed — stale included, because a member invalidated
// while busy keeps serving its last work on the old generation — or the
// one retiring out of the pool while that work finishes. Nil when the
// key has no member at all: never assembled, or fully torn down. An
// invalid key has none either; Current is a read, and a read has
// nothing to refuse.
func (p *Pool[T]) Current(k Key) *T {
	if !k.Valid() {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.pooled[k]; e != nil {
		return e.value
	}
	if e := p.retiring[k]; e != nil {
		return e.value
	}
	return nil
}

// Stale reports whether the generation serving k — or the one that
// served it until a moment ago — was invalidated, i.e. whether a
// replacement is owed. The answer outlives the member's retirement on
// purpose: a caller that was handed the member may ask a moment after
// it retired, and "stale" is the cue to arm a replacement (see
// ScheduleReplacement); answering "no" there would leave the key
// unserved until the next call. A freshly assembled member clears it.
func (p *Pool[T]) Stale(k Key) bool {
	if !k.Valid() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.stale[k]
	return ok
}

// Release drops one Acquire reference. A reference is what holds a
// member against the retirement its invalidation asked for — it is not
// what keeps the member alive — so the release that takes the last one
// retires the member when the key is stale, and leaves it pooled when
// it is not: a member nothing invalidated is the pool's to keep, and
// the next call is served by the same generation.
//
// The last release is not gated on Busy: an application that checks a
// member out is saying with its release that the caller is done with
// it, and an application still working through the member owes that
// release afterwards rather than before it — the pool cannot tell a
// Busy that is right from one that never clears. An idle member that a
// caller holds is the mirror image, and it is retired here rather than
// at its invalidation (see Invalidate).
//
// Releasing a member this pool does not hold, and releasing one it only
// lent out — a member whose references are not out with anyone — are
// both no-ops, so a double Release cannot retire a generation twice.
func (p *Pool[T]) Release(k Key, v *T) {
	if v == nil || !k.Valid() {
		return
	}
	p.mu.Lock()
	e := p.pooled[k]
	if e == nil || e.value != v || e.refs == 0 {
		p.mu.Unlock()
		return
	}
	e.refs--
	if e.refs > 0 {
		p.mu.Unlock()
		return
	}
	if _, stale := p.stale[k]; !stale {
		// Nothing asked for a rebuild: the member stays pooled and its
		// teardown waits for an invalidation, exactly as it would have
		// if this call had never checked it out.
		p.mu.Unlock()
		return
	}
	delete(p.pooled, k)
	p.retireLocked(k, e)
	p.mu.Unlock()
	p.closeMember(k, e)
}

// Settle reports that a member became idle: its last unit of work
// ended. It is what retires a member invalidated while busy — the
// generation stops serving when the work it kept alive ends, not at
// invalidation time, so a second member is never assembled under a live
// unit. A member that was not invalidated stays pooled, a key whose
// member is not v is a no-op, and so is a member a caller still holds:
// retirement waits for the release that is owed for it.
func (p *Pool[T]) Settle(k Key, v *T) {
	if v == nil || !k.Valid() {
		return
	}
	p.mu.Lock()
	e := p.pooled[k]
	if e == nil || e.value != v {
		p.mu.Unlock()
		return
	}
	if _, stale := p.stale[k]; !stale {
		p.mu.Unlock()
		return
	}
	if e.refs > 0 {
		// The work ended, but a caller is still using the member: it
		// stays pooled, and the release that takes the last reference
		// retires it (see Release).
		p.mu.Unlock()
		return
	}
	delete(p.pooled, k)
	p.retireLocked(k, e)
	p.mu.Unlock()
	p.closeMember(k, e)
}

// Invalidate marks one key's member stale: the rebuild path. An idle
// member that no caller holds closes immediately. A member that has work
// in flight, and one a caller checked out, stay pooled and keep serving
// on the old generation — the work ending retires a member nobody holds
// (see Settle), and the last release retires one a caller does (see
// Release). Deferring the swap is what keeps a second member — and a
// second generation of whatever the member holds — from being assembled
// under a live unit or under the caller that is using one.
//
// A key with no pooled member is ignored: there is nothing to retire,
// and the calls that must refuse their input (Ensure, Acquire) are the
// ones that report a key's absence as an error.
func (p *Pool[T]) Invalidate(ctx context.Context, k Key) {
	if !k.Valid() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	e := p.pooled[k]
	if e == nil {
		p.mu.Unlock()
		return
	}
	// Mark the key stale before asking whether the member is busy: a
	// Settle racing this call finds the mark and retires the member
	// instead of leaving a retired generation in the pool.
	p.stale[k] = struct{}{}
	v := e.value
	p.mu.Unlock()

	busy := p.busy(v)

	p.mu.Lock()
	removed := false
	// The gate is the one Settle and Release apply: nothing is working
	// on the member (busy, the application's report) and nobody holds it
	// (refs, the pool's own record). A member a caller is still using is
	// not closed under that caller.
	refs := 0
	if cur := p.pooled[k]; cur == e {
		refs = e.refs
		if refs == 0 && !busy {
			delete(p.pooled, k)
			removed = true
			p.retireLocked(k, e)
		}
	}
	p.mu.Unlock()
	// The reason names what asked for the rebuild, not what is being
	// torn down: a storm of invalidations all pointing at one caller is
	// the signal this line exists for. The other half of the question —
	// why a deferred swap waited — is what the flags answer: work in
	// flight (in_turn) or a caller still holding the member (refs).
	telemetry.Info(ctx, "pool: member invalidated",
		attribute.String("reason", reasonFrom(ctx)),
		attribute.String("key", k.String()),
		attribute.Bool("in_turn", busy),
		attribute.Bool("deferred", !removed),
		attribute.Int("refs", refs),
		attribute.String("member_ptr", fmt.Sprintf("%p", v)))
	if removed {
		p.closeMember(k, e)
	}
}

// InvalidateKind invalidates every pooled member of one kind, and
// nothing else: a workspace-shaped change never touches an
// application's member, and the other way around. Matching runs under
// the pool lock; the retirement itself does not.
func (p *Pool[T]) InvalidateKind(ctx context.Context, kind string) {
	p.invalidate(ctx, func(k Key) bool { return k.Kind == kind })
}

// InvalidateAll invalidates every pooled member, whatever its kind.
func (p *Pool[T]) InvalidateAll(ctx context.Context) {
	p.invalidate(ctx, func(Key) bool { return true })
}

// Close invalidates every pooled member: idle ones close now, members
// with work in flight finish it on the old generation and close after
// the last unit ends. It returns without waiting for that drain. The
// pool stays usable afterwards — a later Ensure assembles again — which
// is what the reload path needs from it, and what makes Close the
// "retire everything" verb rather than a shutdown state.
func (p *Pool[T]) Close() {
	p.InvalidateAll(context.Background())
}

// invalidate collects the keys whose member matches, then retires them
// through the single-key path.
func (p *Pool[T]) invalidate(ctx context.Context, match func(Key) bool) {
	p.mu.Lock()
	keys := make([]Key, 0, len(p.pooled))
	for k := range p.pooled {
		if match(k) {
			keys = append(keys, k)
		}
	}
	p.mu.Unlock()
	for _, k := range keys {
		p.Invalidate(ctx, k)
	}
}

// install applies the configure-once hook to a member the pool is
// handing out. The apply-once marker rides the pool entry, so a member
// the pool has already forgotten is skipped instead of wired late — it
// is closing, and the hand-out paths do not return one of those. The
// hook runs without the pool lock held: it is the application's, and it
// may ask the pool questions of its own.
func (p *Pool[T]) install(k Key, v *T) {
	if p.spec.Installed == nil || v == nil {
		return
	}
	p.mu.Lock()
	e := p.pooled[k]
	if e == nil || e.value != v || e.installed {
		p.mu.Unlock()
		return
	}
	e.installed = true
	p.mu.Unlock()
	p.spec.Installed(k, v)
}

// closeMember begins a member's teardown and, when there is anything to
// wait for, watches for its completion, so the pool learns that the
// member is down — and wakes everyone waiting for it — without anyone
// asking. It is the only path that calls the application's Close, and
// it never runs under the pool lock: a Close that blocks (a drain that
// waits for a lock, a store that closes slowly) must not hold the pool
// up, and its return must not be mistaken for a teardown that has
// already finished.
func (p *Pool[T]) closeMember(k Key, e *entry[T]) {
	p.spec.Close(e.value)
	if p.spec.WaitClosed == nil {
		// No hook to ask: Close returning is the application's word
		// that the member is down.
		p.finishRetiring(k, e)
		return
	}
	go func() {
		// This wait has no deadline of its own: the drain ends with the
		// key's last unit of work, and there is no caller to cancel it.
		ctx := context.WithoutCancel(context.Background())
		_ = p.spec.WaitClosed(ctx, e.value)
		p.finishRetiring(k, e)
	}()
}

// finishRetiring records that a retiring member's teardown finished: the
// entry is dropped — so the next caller assembles its replacement
// instead of waiting on a member that is already gone — and the entry's
// signal is set, waking everyone waiting on that member. The guards make
// both idempotent, so the paths that can observe the same completion do
// not have to coordinate.
//
// The drop comes before the wake-up on purpose: a woken waiter looks at
// the key again, and it should not find the member it just finished
// waiting for still recorded — that reads as "wait on this one", and it
// would spin until the drop landed instead.
func (p *Pool[T]) finishRetiring(k Key, e *entry[T]) {
	p.mu.Lock()
	if p.retiring[k] == e {
		delete(p.retiring, k)
	}
	p.mu.Unlock()
	close(e.closed)
}

// retireLocked records a member that is leaving the pool. The caller
// must hold the lock, must have removed the member from the pool first,
// and starts its teardown after unlocking.
func (p *Pool[T]) retireLocked(k Key, e *entry[T]) {
	if p.retiring == nil {
		p.retiring = make(map[Key]*entry[T])
	}
	if p.retiring[k] != nil {
		// One member serves a key at a time, so a key is never seen
		// retiring two of them; should that ever happen, the second
		// member's teardown still starts — a member whose Close never
		// runs is a leak, which is worse than a bookkeeping accident —
		// while the recorded retirement, the one the key's waiters are
		// waiting on, stays as it is.
		return
	}
	p.retiring[k] = e
}

// waitMember blocks until a member's teardown finished, within ctx.
// Every wait in the pool goes through the entry's own signal, so the
// answer does not depend on where the application's Close happens to
// run: a teardown in flight, one that has not begun yet and one that is
// over are waited for the same way, and a Close that has not returned
// yet cannot read as a finished teardown.
func (p *Pool[T]) waitMember(ctx context.Context, e *entry[T]) error {
	select {
	case <-e.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// busy asks whether the member still has work in flight. It runs
// outside the pool lock: Busy is the application's hook, and a hook
// that asks the pool something must not be made to wait for the pool to
// let go.
func (p *Pool[T]) busy(v *T) bool {
	return p.spec.Busy != nil && p.spec.Busy(v)
}

// open runs the application's assembly for one member, outside every
// pool lock, and logs it: one line per assembly, carrying the key's
// assembly sequence in this pool. That is the line that turns "opens
// are slow" into "this key was built four times in a minute".
func (p *Pool[T]) open(ctx context.Context, k Key) (*T, error) {
	started := time.Now()
	v, err := p.spec.Open(ctx, k)
	duration := time.Since(started)
	if err == nil && v == nil {
		err = ErrNoMember
	}
	if err != nil {
		telemetry.WarnErr(ctx, "pool: member assembly failed", err,
			attribute.String("reason", reasonFrom(ctx)),
			attribute.String("key", k.String()),
			attribute.Int64("duration_ms", duration.Milliseconds()))
		return nil, err
	}
	p.mu.Lock()
	if p.assemblies == nil {
		p.assemblies = make(map[Key]int)
	}
	p.assemblies[k]++
	seq := p.assemblies[k]
	p.mu.Unlock()
	telemetry.Info(ctx, "pool: member assembled",
		attribute.String("reason", reasonFrom(ctx)),
		attribute.String("key", k.String()),
		attribute.Int("assembly_seq", seq),
		attribute.Int64("duration_ms", duration.Milliseconds()))
	return v, nil
}

// assembleShared runs the one assembly for a key and publishes its
// result: the member enters the pool (or is torn down when another
// caller installed one meanwhile), and every waiting Acquire is woken.
// The pool is updated before the wake-up, so a follower either finds
// the pooled member on its next pass or replays the shared error.
//
// checkout is the leader's own: a member published by an Acquire carries
// that caller's reference, while one published by Ensure carries none —
// publication itself is not a hand-out, and a reference nobody owes a
// Release for would keep the member from ever retiring.
func (p *Pool[T]) assembleShared(
	ctx context.Context, k Key, call *assemblyCall, checkout bool,
) (v *T, err error) {
	// One exit point publishes the result: the in-flight entry goes away,
	// the error is recorded, and the waiters are released, whatever the
	// assembly did.
	defer func() {
		p.mu.Lock()
		delete(p.assembling, k)
		call.err = err
		p.mu.Unlock()
		close(call.done)
	}()
	v, err = p.open(ctx, k)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if e := p.pooled[k]; e != nil {
		if checkout {
			e.refs++
		}
		existing := e.value
		p.mu.Unlock()
		// The freshly assembled member never reached the pool and has no
		// work of its own; tear it down outside the pool lock, and wait
		// for that teardown so the caller does not return into a second
		// generation still releasing what it held.
		p.discard(ctx, v)
		return existing, nil
	}
	// A freshly assembled member is current: whatever invalidation the
	// key had seen is answered by this generation.
	delete(p.stale, k)
	e := newEntry(v)
	if checkout {
		e.refs = 1
	}
	p.pooled[k] = e
	p.mu.Unlock()
	return v, nil
}

// discard tears down a member the pool never published.
func (p *Pool[T]) discard(ctx context.Context, v *T) {
	p.spec.Close(v)
	if p.spec.WaitClosed == nil {
		return
	}
	_ = p.spec.WaitClosed(context.WithoutCancel(ctx), v)
}
