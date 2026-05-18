// Package background provides sdk-internal lifecycle primitives for long-lived
// background work.
//
// The package intentionally knows nothing about history, knowledge, recall, or
// telemetry backends. It only fixes the cross-cutting concurrency contract:
// owners create a cancellable context, start goroutines with Add-before-go
// ordering, debounce work through owner-loop signals, and classify work item
// outcomes consistently.
package background
