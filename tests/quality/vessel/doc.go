// Package vesselquality holds in-process integration tests for
// the vessel runtime. Tests construct a Captain with a custom
// engine factory backed by a self-built fake LLM (see ./fakellm)
// so the suite never touches a real provider, never opens a
// socket, and runs deterministically in CI.
//
// Why not unit-test inside vessel/?
//   - vessel/*_test.go covers individual functions; this module
//     covers end-to-end runtime behavior (admission gate, kanban
//     dispatch, restart policy, error propagation) where the
//     wiring is what we want to validate.
//   - Quality tests can pull arbitrary sdk packages without
//     bloating vessel's go.mod or its tagged surface area.
//   - Failures here block tagging vessel; failures in vessel/
//     unit tests block the next sdk patch. Splitting the lanes
//     keeps signal sharp.
package vesselquality
