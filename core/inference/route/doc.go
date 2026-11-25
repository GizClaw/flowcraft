// Package route selects among several providers for one mode call.
//
// # What is routed
//
//   - Generation: ordered candidates, per-target retries, failover, and a trace
//     of every attempt. Streaming fails over only while the call is opening: an
//     error that arrives mid-stream has already produced events, so the
//     dispatcher cannot replay it.
//   - Judgment (decision): the same, with one rule that matters — one call is
//     answered by one model. A retry re-asks every question on the next target;
//     it never mixes answers, because probabilities and confidences from two
//     models are not comparable.
//
// # What is not routed
//
// Embedding. A vector is only meaningful in the space of the model that
// produced it: rerouting a query to a different embedding model returns nearest
// neighbours from a different coordinate system, silently. Embedding may retry
// the same model; changing models requires rebuilding the index, and that is a
// host decision, not a routing one.
//
// # What a target must declare
//
// The dispatcher asks two questions before a target is tried. Does its catalog
// declare the requested model id, and does that model serve what the call asks for —
// the output mode, the tool channel, an embedding, a judgment? Input content kinds
// are checked the same way, because a fleet-wide mismatch is a configuration fact
// rather than a provider fault.
//
// The questions stop where a declaration stops: a target that publishes no line-up is
// assumed to generate text, and a declaration that says nothing filters nothing. The
// driver is still the final arbiter of everything a declaration does not cover, and
// its refusals are classified as "try the next target".
//
// # A session restricts the fleet
//
// [Router.Restrict] returns a copy that only serves a listed set of models, in order,
// with the modes each may be used for. The host builds one per session from the
// caller's grant, so authority is enforced by the same code that picks the target:
// a model outside the list is refused with a classified error before any provider
// sees the call, and a request that names no model is answered from the list.
package route
