// Package delegation provides the local delegation runtime over deploy.Result.
//
// Directory borrows a deploy result for stable target discovery. Service
// executes sync requests directly, represents handoff as control transfer, and
// delegates async persistence to a backend-neutral AsyncBackend.
package delegation
