// Package route selects among several providers for one seam call.
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
// # Discovery is the driver's job
//
// The dispatcher does not ask whether a target can serve a request. A driver
// rejects an undeclared model or an unsupported feature during compilation,
// before any provider I/O, and those kinds are classified as "try the next
// target". Capability filtering therefore lives where the knowledge is — in each
// driver's declarations — not in a table here.
package route
