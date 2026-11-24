// Package delegation is the delegate tool: the model's way to hand work to another agent.
//
// It is a tool and nothing else, which is why it lives in the tool module beside the other sources
// rather than in core: opening the child, admitting the edge against the session's policy, running the
// turn and folding its result are the host's. A tool may not reach the host, so what this package holds
// is a schema, an argument decode, and a call through the [platform.Delegation] capability the host
// filled for the session.
//
// That split is why the tool is small and why it can be tested without a host: the capability is an
// interface, and a test supplies one.
package delegation
