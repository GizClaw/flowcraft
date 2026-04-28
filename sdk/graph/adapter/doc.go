// Package adapter bridges graph.Graph onto the workflow.Strategy interface so
// graph-defined runs can be hosted by the legacy workflow.Runtime.
//
// Deprecated: this package exists solely to wire graph into workflow.Strategy,
// which is itself scheduled for removal in v0.3.0. New code should consume the
// graph engine directly via graph/runner.Runner. This package will be removed
// alongside workflow in v0.3.0.
package adapter
