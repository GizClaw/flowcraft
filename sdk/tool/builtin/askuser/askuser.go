// Package askuser exposes the built-in `ask_user` LLM tool — a
// human-in-the-loop bridge that lets the model explicitly hand a
// question back to the operator via [engine.Host.AskUser].
//
// Deprecated: moved to sdkx/tool/askuser. The new canonical import
// path is "github.com/GizClaw/flowcraft/sdkx/tool/askuser". This
// package continues to host the implementation only because sdk
// cannot import sdkx (the dependency runs one-way); the sdkx path
// is a thin forwarder during the migration window. Will be removed
// in v0.5.0 — same window as catalog.Deps.AgentTools,
// runner.WithActorKey, and workspace.CommandRunner. Migrate via a
// pure import-path swap:
//
//	-import "github.com/GizClaw/flowcraft/sdk/tool/builtin/askuser"
//	+import "github.com/GizClaw/flowcraft/sdkx/tool/askuser"
//
// Without a real consumer, host.AskUser is a dead capability: the
// engine.NoopHost rejects it, scriptnode forwards a script-side
// API to it, but no model-driven path ever called it. ask_user
// closes that gap so a graph composed of an llmnode + this
// registered tool can perform "I need a clarification from the
// human" turns end-to-end.
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
// Execute recovers it via engine.HostFromContext and forwards
// the LLM-supplied prompt to host.AskUser. The host's
// UserPrompter implementation (typically a UI controller, a
// queued kanban card, or a terminal prompt) returns the human's
// reply, which surfaces back to the LLM as the tool result body.
//
// # Capability gating
//
// Engines that include this tool in their advertised registry
// implicitly emit user prompts. Hosts SHOULD declare
// engine.Capabilities.EmitsUserPrompt = true so the runtime can
// route those prompts to a real user-facing surface; an embedded
// fire-and-forget batch run that wires only NoopHost will see
// every ask_user call surface as errdefs.NotAvailable. That is
// honest behaviour: the model asked a question nobody can
// answer, and the surface error tells the LLM exactly that.
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

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// Name is the canonical tool id callers register and LLMs invoke.
// Stable across versions so prompts referring to the tool by name
// keep working.
//
// Deprecated: use sdkx/tool/askuser.Name. Will be removed in v0.5.0
// (same window as catalog.Deps.AgentTools, runner.WithActorKey, and
// workspace.CommandRunner). The value is preserved verbatim across
// both paths so prompts / registry lookups keep working.
const Name = "ask_user"

// args is the wire-side argument struct. JSON-only; no positional
// form. The schema mirrors this exactly (see Definition below).
type args struct {
	Prompt string `json:"prompt"`
}

// askUserTool implements tool.Tool. Stateless; safe to register
// once and share across runs.
type askUserTool struct{}

// New constructs the built-in ask_user tool. The returned value
// satisfies tool.Tool and can be passed to Registry.Register.
//
// Deprecated: use sdkx/tool/askuser.New. Will be removed in v0.5.0
// (same window as catalog.Deps.AgentTools, runner.WithActorKey, and
// workspace.CommandRunner). The sdkx forwarder returns the exact
// same tool implementation, so a pure import-path swap is enough.
func New() tool.Tool { return askUserTool{} }

// Definition returns the model-facing schema. Description is the
// LLM's only hint for when to use it: keep it conservative —
// "ask the human only when truly needed" — to discourage chatty
// models from interrupting on every minor uncertainty.
func (askUserTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name: Name,
		Description: "Ask the human user a clarifying question and " +
			"wait for their reply. Use only when you genuinely " +
			"cannot proceed without their input — most questions " +
			"can be answered from context. Returns the user's reply " +
			"as a string.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{
					"type":        "string",
					"description": "The question to display to the user.",
				},
			},
			"required":             []string{"prompt"},
			"additionalProperties": false,
		},
	}
}

// Execute parses the LLM-supplied arguments, recovers the engine
// host from ctx, and forwards the prompt to host.AskUser. Errors
// are mapped to errdefs categories so callers (LLM round, tool
// telemetry) can classify them with errdefs.Is*.
func (askUserTool) Execute(ctx context.Context, arguments string) (string, error) {
	var a args
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", errdefs.Validationf("ask_user: parse arguments: %v", err)
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return "", errdefs.Validationf("ask_user: prompt must be non-empty")
	}

	host, ok := engine.HostFromContext(ctx)
	if !ok || host == nil {
		// No host on ctx means the tool is running outside an
		// engine that wired one up (raw test path, batch run
		// with NoopHost). Surface NotAvailable so the LLM sees
		// "this is not currently a supported capability" rather
		// than crashing or returning nonsense.
		return "", errdefs.NotAvailablef("ask_user: no engine.Host on ctx; did the engine wire it via engine.WithHost?")
	}
	prompt := engine.UserPrompt{
		Parts:  []model.Part{{Type: model.PartText, Text: a.Prompt}},
		Source: Name,
	}
	reply, err := host.AskUser(ctx, prompt)
	if err != nil {
		return "", err
	}
	return replyText(reply), nil
}

// replyText collapses the host's reply into the single string the
// LLM tool surface expects. We concatenate all text parts in
// order, separated by newlines; non-text parts (image / audio /
// file) are summarised by their Type so the model at least
// learns "the user attached a thing of type X" rather than
// silently dropping non-text replies. Hosts that need richer
// shapes should wrap the tool with their own custom variant.
func replyText(r engine.UserReply) string {
	var b strings.Builder
	wrote := false
	for _, p := range r.Parts {
		if wrote {
			b.WriteByte('\n')
		}
		switch p.Type {
		case model.PartText:
			b.WriteString(p.Text)
		default:
			// Non-text part: write a minimal marker. We
			// deliberately avoid base64-blobbing media into
			// the model context — that would balloon token
			// counts for no immediate gain.
			b.WriteString("[user attached a non-text part: ")
			b.WriteString(string(p.Type))
			b.WriteString("]")
		}
		wrote = true
	}
	return b.String()
}

// Compile-time assertion the tool satisfies the contract. Keeps
// signature drift in sdk/tool from silently breaking the built-in.
var _ tool.Tool = askUserTool{}
