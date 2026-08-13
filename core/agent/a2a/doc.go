// Package a2a adapts a remote A2A (Agent2Agent) agent into a FlowCraft
// [agent.Engine].
//
// The package is a client-side proxy: one [Engine.Execute] call proxies a
// FlowCraft turn to a remote A2A agent over JSON-RPC (HTTP) or gRPC and maps
// the remote task lifecycle back onto the [agent.Board] / [agent.Host]
// contract. It speaks both the A2A v0.3 JSON-RPC wire format (camelCase,
// lowercase enums) and A2A v1.0 (proto-normalised, uppercase enums, oneof
// stream responses); the transport for a run is selected automatically from
// the remote AgentCard's declared protocol version and interfaces, or pinned
// through [WithStreamMode] / config settings.
//
// # Layering
//
//   - The wire protocol is owned by the official A2A Go SDK
//     ([github.com/a2aproject/a2a-go/v2]): protocol types come from its
//     a2a package, JSON-RPC transports from a2aclient / a2acompat/a2av0,
//     and gRPC transports from a2agrpc/v0 and a2agrpc/v1. This package
//     implements no wire code of its own.
//   - [Engine] implements [agent.Engine] plus [agent.Resumer]; it is
//     registered with deployment tooling through config.Factory
//     (Kind "a2a") in the config subpackage.
//
// # Capabilities
//
// The engine reports SupportsResume (checkpoints carry the remote task id),
// EmitsCheckpoint (it stamps checkpoints at task boundaries) and
// EmitsUserPrompt (A2A "input-required" states are bridged through
// [agent.Host.AskUser]).
package a2a
