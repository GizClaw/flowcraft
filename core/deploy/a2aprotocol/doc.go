// Package a2aprotocol exposes an agent over the A2A protocol.
//
// It is the built-in protocol kind of core/deploy, and it lives next to deploy
// because deploy is its only host: a server is assembled from a deployment's
// session table, checkpoint store and identity. Those arrive as values, so this
// package never imports deploy and deploy can register it without a cycle.
//
// Kind registers the exposure with deploy; the rest of the package is the
// server itself.
//
// The package owns the A2A translation — text as one artifact per model round,
// status as the task's state, produced artifacts as fetchable parts — and the
// durable task store that keeps tasks/get answering after a restart.
package a2aprotocol
