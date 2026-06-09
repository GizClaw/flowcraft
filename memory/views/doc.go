// Package views defines the shared vocabulary for derived memory views.
//
// The root package is a local contract layer. It names a view, names canonical
// evidence it cites, records the source revisions and upstream view outputs used
// to produce one output, and provides stable-key and local validation helpers for
// those values.
//
// This package is intentionally not a view framework. It does not rebuild,
// reconcile, drain, shut down, report readiness, own retrieval namespaces, or
// validate a complete lineage DAG. Concrete view packages, recipe assembly,
// lifecycle planners, diagnostics, and retrieval infrastructure own those
// responsibilities.
package views
