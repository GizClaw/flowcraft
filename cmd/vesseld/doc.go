// Command vesseld is the FlowCraft Vessel orchestration daemon. It
// loads a folder of declarative configuration (apiVersion +
// kind-style YAML/JSON), wires up shared LLM clients / probes /
// tool packs / history stores, hosts N vessel.Captain instances in
// the same process, and exposes an HTTP control plane for Submit /
// Call / Logs / Phase / Drain / Stop.
//
// vesseld is the application-level pairing for the vessel/ runtime
// library — pure Go consumers can keep using vessel.Captain
// directly; vesseld is for users who want a configuration-driven,
// long-running daemon with a wire API.
//
// The short version:
//
//   - one daemon process hosts many Vessels (cross-vessel LLM
//     client + rate-limit sharing is the core efficiency win)
//   - configuration is a folder; loading happens once at startup
//   - the schema is versioned (vessel.flowcraft.io/v1alpha1) and
//     v0.1.0 ships 8 kinds: Daemon, Vessel, Agent, LLMProfile,
//     Probe, ToolPack, HistoryStore, Secret
//   - default control plane is an HTTP server bound to a Unix
//     socket; TCP requires explicit token auth
//
// CLI:
//
//	vesseld run --config DIR [-R]       start the daemon
//	vesseld validate --config DIR [-R]  schema + ref check, no IO
//	vesseld plan --config DIR [-R]      print resolved Plan (secrets redacted)
//	vesseld version                     module versions
//
// vesseld is its own Go module (cmd/vesseld/go.mod) so daemon
// dependencies (yaml.v3, http) do not leak into the SDK or the
// vessel runtime library. Build it with `go build ./cmd/vesseld`
// from the cmd/vesseld directory or via the top-level Makefile.
package main
