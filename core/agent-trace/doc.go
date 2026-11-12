// Package agenttrace reads a finished task's record log back as a timeline.
//
// The store holds two things a reader needs and neither alone is enough: the
// projection (the Task row, which answers "where does this stand now") and the
// append-only event log (which answers "how did it get there"). This package
// joins them, decodes what the engines wrote, and hands back an ordered, dated
// list of facts.
//
// # What it is not
//
// It is not an engine: it never runs a node and never writes. It is not the
// runtime's job either — a deployment does not read its own log, a host and a
// CLI do — which is why it sits above both and is imported by neither.
//
// # Two readers
//
// An operator asks "what happened, in what order, how long did it take" and
// wants [WriteText]. A dataset asks "which prompt, which model, what did it
// cost" and wants the typed views: [Trace.Calls], [Trace.Effects], [Trace.Usage],
// [Trace.State].
//
// # Failure semantics
//
// A reader that refuses to show a run because one record came from another
// engine — or because a payload changed shape between two versions — is useless
// for the incident it was fetched for. So:
//
//   - a kind no decoder owns is kept as a [Fact] with its raw payload, not dropped;
//   - a payload that does not decode is the same case: the decoder reports false
//     and the fact stays opaque;
//   - a zero At means "unknown", because rows written before events carried a
//     time have none, and inventing one would place them first on every timeline;
//   - only an unreadable store or a missing task fails the read, and the latter
//     is [agent.ErrNoCheckpoint], the store's own answer.
package agenttrace
