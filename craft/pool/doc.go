// Package pool keeps one member per key alive across generations:
// which key gets which member, when an invalidated member is retired,
// who assembles its replacement, and how callers wait that swap out.
//
// It exists because the same state machine was written — and then
// debugged — inside an application. The retiring / assembling
// bookkeeping, the Ensure / Acquire split, the one deferred
// replacement per key, the configure-once hand-out and the bounded
// cross-generation retry are the same whatever a member is made of, so
// they live here, once. What a key means, what a member is built from
// and when the application asks for one stay on the application side
// (see Spec).
//
// The pool knows nothing about Craft or runtimes: T is the value one
// key holds, and the Spec says how it is built, when it is busy and
// how its teardown finishes. The pool itself is not built into Craft
// either — an application that pools one Craft runtime per workspace
// or application id gets its pool from here and keeps its identities,
// its invalidation triggers and its wiring.
//
// Two entrance points, not one:
//
//   - Ensure is the cheap read path, called once per unit of work. It
//     answers "the member that serves this key now": the pooled one —
//     including one that was invalidated but is still serving its last
//     work on the old generation — or a fresh assembly. It takes no
//     reference.
//   - Acquire checks a member out: it waits out a retiring member's
//     teardown instead of handing out one that is on its way out, and
//     the caller owes a Release.
//
// What a reference buys is not the member's life but its stay: a member
// is retired when its key is invalidated — at once for an idle one,
// once the work it kept alive ends for a busy one, and at the last
// reference for one that is still checked out. A member nothing
// invalidated is the pool's to keep, so the release that takes the last
// reference leaves it pooled and the next caller is served by the same
// generation. The pool owns that decision; Ensure, which takes no
// reference, only ever borrows against it.
//
// A key never has two members at once. While one drains, Acquire
// waits; while one is being assembled, concurrent callers share the
// single assembly and its error. A member's teardown is waited for
// through the pool's own record of it, so "Close returned" and
// "teardown finished" stay distinct for the application too: the
// signal is set once the application's WaitClosed, where it has one,
// has finished.
package pool
