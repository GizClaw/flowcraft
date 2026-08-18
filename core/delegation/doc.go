// Package delegation defines backend-neutral contracts for assigning work to
// another agent or execution target.
//
// Mode distinguishes synchronous and asynchronous work. Directory owns
// discovery; Service owns execution and status lookup. The local service
// (LocalService), the deploy-bound directory (LocalDirectory), and the
// delegation resource factories live in this package; the model-facing
// delegate / delegation_status tools live in the tool subpackage, and the
// runtime host-factory integration (exposing a Service on every turn host)
// lives in the hostwrap subpackage.
//
// A host can expose a Service to execution-time tools with [WithService],
// and consumers recover it with [ServiceFromHost].
package delegation
