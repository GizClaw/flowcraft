// Package askuser is the canonical home for the `ask_user` LLM tool
// — a human-in-the-loop bridge that lets the model explicitly hand a
// question back to the operator via [engine.Host.AskUser].
//
// # Why sdkx
//
// sdk defines interfaces and primitives; sdkx ships concrete
// adapters that integrate with external systems or external
// protocol specs. tool.Tool implementations are concrete adapters
// — they bridge the generic tool.Tool interface to one specific
// service — and therefore belong here, mirroring the existing
// sdkx/tool/anthropicmemory and sdkx/tool/exec layouts.
//
// # Wiring
//
// Register the tool into the same tool.Registry the LLM node
// already consults:
//
//	reg := tool.NewRegistry()
//	reg.Register(askuser.New())
//
// At round time, llmnode stashes the engine.Host on ctx via
// engine.WithHost before invoking reg.ExecuteAll. The tool's
// Execute recovers it via engine.HostFromContext and forwards the
// LLM-supplied prompt to host.AskUser. The host's UserPrompter
// implementation (typically a UI controller, a queued kanban
// card, or a terminal prompt) returns the human's reply, which
// surfaces back to the LLM as the tool result body.
//
// # Capability gating
//
// Engines that include this tool in their advertised registry
// implicitly emit user prompts. Hosts SHOULD declare
// engine.Capabilities.EmitsUserPrompt = true so the runtime can
// route those prompts to a real user-facing surface; an embedded
// fire-and-forget batch run that wires only NoopHost will see
// every ask_user call surface as errdefs.NotAvailable — that is
// honest behaviour: the model asked a question nobody can answer,
// and the surface error tells the LLM exactly that.
//
// # Wire shape
//
// Arguments (JSON object):
//
//	{
//	  "prompt": "string, the question to ask the user (required)"
//	}
//
// Result: the human's reply as a plain string. Errors:
//
//   - errdefs.Validation: arguments did not parse / prompt empty.
//   - errdefs.NotAvailable: no engine.Host on ctx, or the host
//     refused the prompt (UserPrompter returned the same).
//   - any other error: forwarded verbatim from host.AskUser.
package askuser
