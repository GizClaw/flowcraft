// Package deploy assembles a deployment from a resource+agent document:
// the [Document] DTO (resources forming a dependency DAG, agents
// binding them), and the [Builder] with two phases: Build constructs
// resources in topological order, Wire attaches observers and hooks
// and binds agents. [Builder.Deploy] runs both.
//
// The package imports only core packages: the resource protocol
// ([resource]) and error classification ([errdefs]). It knows no
// concrete resource kind.
package deploy
