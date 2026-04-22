// Package pipeline composes pluggable retrieval Stages over a retrieval.Index
// .
//
// A Pipeline is a linear list of Stages run sequentially over a shared State.
// Stages may short-circuit by setting State.ShortCircuit = true (e.g. native
// hybrid backends).
package pipeline
