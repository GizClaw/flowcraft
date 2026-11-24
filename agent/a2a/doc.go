// Package a2a drives a remote agent as an engine.
//
// A peer is a deployment's program exactly as a compiled graph is: [New] connects to it once,
// [Program.Open] binds it to a session, and the driver it returns forwards one message per turn and
// streams the peer's events back as [agent.Event]s. The host does not know which of the two it is
// running.
//
// Three things are deliberately different from a local engine.
//
// It is not steerable. Steering is an operator's channel into a run in flight, and the A2A protocol
// has no such message: an adapter that accepted input it could never deliver would be lying about a
// capability, so the driver does not implement [agent.Steerer] at all.
//
// It is cancellable in the protocol's sense. Dropping the turn's context stops the local read but
// leaves the peer running, so [driver.Cancel] sends the protocol's own cancellation; that is what
// [agent.Canceller] exists for.
//
// It owns no task of its own. The task the host minted is the host's — it names the session and it
// is what a resume looks up — so the peer's task id and context live in a section of that task's
// checkpoint. Sections merge across saves, which is what lets this driver write its own section
// while the turn decorator writes the terminal state of the same task.
//
// The wire mapping is a subpackage ([wire]) rather than a file here: the half that serves A2A on the
// other side must map the same fields, and one implementation is the only thing that keeps a media
// part or a metadata key from being decided twice.
package a2a
