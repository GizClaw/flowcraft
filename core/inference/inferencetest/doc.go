// Package inferencetest provides reusable conformance suites for inference
// providers.
//
// Provider packages should keep provider-specific wire assertions in their own
// tests and use these suites for the shared Runtime contracts: Explain must not
// perform provider I/O, execution metadata must retain compiler decisions,
// requests must remain caller-owned, and sessions must compile each input
// before transport.
package inferencetest
