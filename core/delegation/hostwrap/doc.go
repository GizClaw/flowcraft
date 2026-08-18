// Package hostwrap bridges core/delegation and the runtime host factory
// seam. It exposes the delegation service built in a deployment on
// every turn host created by a runtime host factory.
//
// The runtime itself stays delegation-neutral: applications opt in by
// installing a result-aware host factory decorator
// (runtime.Builder.WithResultHostFactory) and delegating to [Wrap].
package hostwrap
