// Package a2a is the A2A wire adapter for core/agent.
//
// It owns three things and nothing else: the AgentCard a client reads, the
// conversion of a Task into A2A JSON, and the mapping of agent events onto A2A
// stream updates. It does not run the loop, and it does not define the internal
// lifecycle: TaskState already uses the A2A names, so a task state crosses this
// boundary unchanged.
//
// The card is a promise to clients. Only two fields come from the runtime —
// streaming, because the loop has no non-streaming form, and the protocol
// version, because it is a constant of this adapter. Everything else is
// deployment metadata: media modes describe what the endpoint accepts (a client
// may send a PDF that is converted to text before any model sees it), and skills
// describe what the agent claims it can do. Deriving those from model
// capabilities would be a claim nothing verifies.
package a2a
