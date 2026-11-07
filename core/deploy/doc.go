// Package deploy owns deployed agents: their runtimes, their conversations, and
// their lifetime.
//
// It sits above core/agent and core/runtime and knows nothing about how a client
// reaches an agent. Its unit is one deployed agent: a runtime built from one
// configuration, plus the conversations that runtime has served.
//
// The only surface it exposes for talking to a conversation is Submit and
// Resume. A raw Session is deliberately not reachable: an undelivered message is
// reported to whoever drives the loop, and if callers could drive it themselves
// they could drop input the user sent and nobody saw.
package deploy
