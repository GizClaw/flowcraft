// Package compiler turns capability-oriented memory specs into internal
// assembly plans.
//
// The package is intentionally not a public recipe or mode API. It is the
// bridge for future YAML/capability configuration to name canonical sources,
// semantic views, indexed projections, and lifecycle stages without exposing
// product scenarios as Go helpers. It does not parse YAML, instantiate stores,
// run workers, materialize projections, or write retrieval documents.
package compiler
