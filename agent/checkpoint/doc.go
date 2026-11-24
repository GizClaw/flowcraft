// Package checkpoint is the platform's durable [agent.Store]: one SQLite file per workspace.
//
// It lives in the agent module, not in core, for the reason every implementation does: core holds the
// contract and this holds a backend that ships with the platform. A host that wants a different store
// — a shared Postgres, an object store — implements agent.Store somewhere else and never imports this
// package or the driver it carries.
//
// It passes [agenttest.StoreSuite]: what a resume relies on is the contract, and the contract has a
// suite, so a backend is verified against it instead of re-deriving what Save and Load mean.
package checkpoint
