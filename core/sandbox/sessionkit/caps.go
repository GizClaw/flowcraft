package sessionkit

// maxCapSampleFailures is how many consecutive sampling errors a cap watcher
// tolerates before declaring the caps unenforceable. One flaky sample (a
// transient fork failure under load, a query that raced the process exiting)
// must not kill an innocent group, but blindness must not be unbounded either:
// at the unix watcher's 250ms cadence three strikes bound it to roughly 750ms,
// and at the Windows sampler's 25ms to roughly 75ms.
//
// The number is shared so the two backends cannot drift: "how long may we be
// blind" is a contract question, not a platform detail. A watcher that gives up
// kills the session and reports sandbox.SessionUnenforceable, because a child
// left running unguarded under a capability that still claims the cap is
// exactly what the cap contract exists to prevent.
const maxCapSampleFailures = 3
