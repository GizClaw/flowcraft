// Package sandbox is the agent's execution boundary: where commands run,
// what they can reach (net), what they can see (env), and how much they
// can consume (resources). Sandbox is daemon-level shared policy; per-run
// state lives in sdk/workspace.
//
// The package centres on the Runner interface, a single Exec call that
// turns a command + arguments + ExecOptions into an ExecResult. Concrete
// runners differ in *where* the work happens (local process, nsjail
// namespace, container, microVM) but share the same policy surface so a
// caller can be retargeted between backends without changing call sites.
//
// ExecOptions carries three policy groups beyond the obvious WorkDir /
// Stdin / Timeout knobs:
//
//   - Env (EnvPolicy): explicit allow-list of host environment variables
//     plus an Inject map. Replaces "inherit the entire daemon's env" which
//     is unsafe in a multi-tenant agent harness.
//   - Net (NetPolicy): mode + (future) allow-list / proxy URL. LocalRunner
//     only accepts NetDefault; non-default modes require a sandboxing
//     backend such as sdkx/sandbox/{nsjail,container,microvm} (planned).
//   - Resources (ResourceLimits): CPU / memory / disk caps plus
//     MaxOutputBytes. LocalRunner only enforces MaxOutputBytes today; the
//     hard caps require kernel-level mechanisms shipped by sdkx backends.
//
// LocalRunner is the in-process, no-isolation backend used by tests and
// single-tenant operators. Production deployments should compose it with
// AllowCommands (whitelist) and eventually swap to a sandboxed Runner
// when sdkx ships them.
package sandbox
