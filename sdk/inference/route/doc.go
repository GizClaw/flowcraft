// Package route provides optional model selection above inference.Runtime.
//
// It owns deployment-defined tiers, normalized model scores, operation-specific
// target pools, selector contracts, and route traces. It does not declare model
// capabilities: selectors return an exact inference.ModelRef, and the provider
// compiler remains the authority on whether the concrete request is executable.
package route
