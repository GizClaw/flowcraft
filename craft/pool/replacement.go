package pool

import (
	"context"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// ScheduleReplacement arms the deferred replacement of one key's member
// and reports whether this call armed it — false means one was already
// armed, i.e. the drain in progress is already accounted for. It is how
// a reload handles a member that is stale but still serving work: the
// work keeps the generation it started on, and a fresh member is
// assembled as soon as that one has finished teardown.
//
// One replacement per key on purpose. Several invalidations can land on
// one key inside a single drain; with a watcher per invalidation they
// all woke at once and assembled a member each — one installed, the
// rest built and thrown away. The single armed watcher assembles from
// whatever the key's Open reads at the time it runs, so a later
// invalidation has nothing left to ask for.
//
// A key that names nothing does not arm anything, and a nil ctx is
// treated as context.Background(); the ctx is carried into the
// assembly, without its cancellation (the watcher outlives the caller
// that armed it).
func (p *Pool[T]) ScheduleReplacement(ctx context.Context, k Key) bool {
	if !k.Valid() {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	if _, ok := p.armed[k]; ok {
		p.mu.Unlock()
		return false
	}
	if p.armed == nil {
		p.armed = make(map[Key]struct{})
	}
	p.armed[k] = struct{}{}
	p.mu.Unlock()
	go p.replaceAfterDrain(context.WithoutCancel(ctx), k)
	return true
}

// ReplacementArmed reports whether a replacement is already scheduled
// for k. A caller deciding whether the member it holds still needs a
// rebuild asks this to tell "this generation is stale and its
// replacement is on the way" from "it is stale and nothing is coming".
func (p *Pool[T]) ReplacementArmed(k Key) bool {
	if !k.Valid() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.armed[k]
	return ok
}

// disarmReplacement releases the armed slot once the drain settled, so
// a later invalidation can arm a new replacement.
func (p *Pool[T]) disarmReplacement(k Key) {
	p.mu.Lock()
	delete(p.armed, k)
	p.mu.Unlock()
}

// replaceAfterDrain waits for the key's retiring member to finish
// teardown, then assembles its replacement. A member that is stale but
// still serving keeps serving new work on its old generation until its
// last unit ends, so the replacement cannot be built any earlier
// without a second member serving the key.
func (p *Pool[T]) replaceAfterDrain(ctx context.Context, k Key) {
	defer p.disarmReplacement(k)
	p.mu.Lock()
	e := p.pooled[k]
	_, stale := p.stale[k]
	var v *T
	switch {
	case e != nil && !stale:
		// A live member serves the key: nothing was draining, so there
		// is no replacement to schedule.
		p.mu.Unlock()
		return
	case e != nil:
		v = e.value
	default:
		// Either a member is retiring — wait it out — or teardown
		// finished between the arm and this watcher, and that second
		// case is exactly what the replacement is for: a key left
		// unserved stays unserved until the next call otherwise.
		v = p.retiring[k]
	}
	p.mu.Unlock()
	if v != nil {
		// WaitClosed only fails on a canceled context, and this wait has
		// no deadline of its own: the drain ends with the key's last
		// unit of work.
		if err := p.waitClosed(ctx, v); err != nil {
			return
		}
	}
	if !p.wanted(k) {
		return
	}
	replacement, err := p.Ensure(WithReason(ctx, ReasonRetryAfterDrain), k)
	if err != nil {
		telemetry.WarnErr(ctx, "pool: deferred rebuild failed", err,
			attribute.String("key", k.String()))
		return
	}
	if replacement == nil {
		return
	}
	if p.spec.Replaced != nil {
		p.spec.Replaced(k, replacement)
	}
}

// wanted consults the application's Wanted hook; a key no hook answers
// for is wanted.
func (p *Pool[T]) wanted(k Key) bool {
	return p.spec.Wanted == nil || p.spec.Wanted(k)
}
