package pool

import (
	"context"
	"errors"
	"time"
)

const (
	// DefaultRetryWindow bounds how long Do waits for a key's member,
	// across all its attempts. It is Spec.RetryWindow's zero value.
	DefaultRetryWindow = 10 * time.Second
	// DefaultRetryAttempts caps Do's attempts. It is
	// Spec.RetryAttempts's zero value.
	DefaultRetryAttempts = 3
)

// Do runs fn against the member that serves k, absorbing the
// application's transient lifecycle guards — the errors Spec.Retryable
// classifies — by waiting, inside one retry window, for the key's
// replacement member and running fn again. It is the one place those
// guards are retried: every caller used to spell out the same window,
// attempt budget and re-resolve sequence.
//
// fn returning nil ends the loop. A non-retryable error ends it too —
// those are the caller's own failures (a busy conversation, an invalid
// id, a validation refusal), and repeating them is pointless. A
// retryable one is retried until the window or the attempt budget runs
// out, and what the caller saw last is what comes back: the guard it
// hit, or ErrNotReady when the key never got a usable member before the
// window closed.
//
// The wait is bounded by the remaining window rather than by the
// caller's whole call: a unit of work that has to cross a drain waits
// at most the window for it, and never for the length of somebody
// else's work. A member that resolves as the window runs out is still
// used — the expiry check only turns an attempt that came back with
// nothing in hand into "not ready".
//
// stop is consulted before every retry, never before the first attempt:
// a caller whose premise expired — the window moved away from the key
// it named — gives up instead of holding the call open for a key nobody
// is looking at, and the first attempt still runs, because it is
// resolved against the key the caller named rather than whatever the
// application happens to show. Nil means "keep trying until the window
// runs out".
//
// A nil ctx is treated as context.Background().
func (p *Pool[T]) Do(
	ctx context.Context,
	k Key,
	stop func() bool,
	fn func(v *T) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(p.retryWindow())
	lastErr := error(ErrNotReady)
	for attempt := 0; attempt < p.retryAttempts(); attempt++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return lastErr
		}
		attemptCtx, cancel := context.WithTimeout(ctx, remaining)
		v, err := p.Ensure(attemptCtx, k)
		// Only the window's own expiry is absorbed below. A caller whose
		// context died (a canceled call, a deadline of its own) keeps its
		// error, and so does an assembly that failed for its own
		// reasons. A window that closed with a member in hand is not an
		// expiry either: the pool resolved one as the wait ran out, and
		// answering "not ready" would throw away a member the caller can
		// use.
		expired := ctx.Err() == nil &&
			errors.Is(attemptCtx.Err(), context.DeadlineExceeded) &&
			(err != nil || v == nil)
		cancel()
		switch {
		case expired:
			// The window closed while the key was still draining. Who
			// waited is not the caller's business: it gets the guard it
			// last hit.
			return lastErr
		case err != nil && !p.retryable(err):
			return err
		case err != nil:
			lastErr = err
		case v == nil:
			// The pool answers "no member for this key" only when it was
			// asked for nothing: wait for one like any other guard.
			lastErr = ErrNotReady
		default:
			lastErr = fn(v)
			if lastErr == nil || !p.retryable(lastErr) {
				return lastErr
			}
		}
		// Retry only while the premise holds: the caller's call is still
		// alive, and (when it named one) its key is still the one it
		// asked about.
		if ctx.Err() != nil || (stop != nil && stop()) {
			return lastErr
		}
	}
	return lastErr
}

// retryable reports whether waiting for a replacement member can fix
// err: the application's classifier plus the pool's own ErrNotReady.
func (p *Pool[T]) retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotReady) {
		return true
	}
	return p.spec.Retryable != nil && p.spec.Retryable(err)
}

// retryWindow is the wait budget Do spends on one call.
func (p *Pool[T]) retryWindow() time.Duration {
	if p.spec.RetryWindow > 0 {
		return p.spec.RetryWindow
	}
	return DefaultRetryWindow
}

// retryAttempts is the attempt budget Do spends on one call.
func (p *Pool[T]) retryAttempts() int {
	if p.spec.RetryAttempts > 0 {
		return p.spec.RetryAttempts
	}
	return DefaultRetryAttempts
}
