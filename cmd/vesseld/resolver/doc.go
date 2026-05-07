// Package resolver translates the apispec Object inventory loaded
// from a --config folder into a fully-realised runtime [Plan]: per-
// vessel spec.Spec values, per-vessel vessel.Option lists,
// the daemon-shared tool registry / LLM resolver / history stores,
// and the secret lookup the vessel runtime uses at startup.
//
// Three principles guide the design:
//
//  1. Pure: Resolve() is allowed to read files referenced by
//     valueFrom.file (since secret loading without IO is a non-
//     starter), but is forbidden from doing any network IO. The
//     `vesseld validate` CLI reuses Resolve with a "skip secret
//     reads" mode so static analysis stays fast.
//  2. Errors aggregate: a typo in one Vessel does not stop
//     validation of the rest. Resolve returns an [Errors] value
//     containing every problem so users see the full picture.
//  3. The Plan is immutable and serialisable: `vesseld plan`
//     prints it (with secret values redacted) for CI diffing and
//     incident debugging. No live socket / FD / goroutine handles
//     belong in the Plan.
package resolver
