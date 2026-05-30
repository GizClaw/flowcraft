// Package ops composes memory/recall's caller-driven processors into
// embeddable operator loops.
//
// The recall core intentionally does not own goroutines or CLI wiring. This
// package keeps that boundary: callers pass an already configured
// recall.Memory, choose the scopes to operate on, and own the returned Runner's
// lifecycle through context cancellation.
package ops
