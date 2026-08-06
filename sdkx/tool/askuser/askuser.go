package askuser

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// Name is the canonical tool id callers register and LLMs invoke.
// Stable across versions so prompts referring to the tool by name
// keep working.
const Name = "ask_user"

// args is the wire-side argument struct. JSON-only; no positional
// form. The schema mirrors this exactly (see Definition below).
type args struct {
	Prompt string `json:"prompt"`
}

// askUserTool implements tool.Tool. Stateless; safe to register
// once and share across runs.
type askUserTool struct{}

// New constructs the ask_user tool. The returned value satisfies
// tool.Tool and can be passed to Registry.Register.
func New() tool.Tool { return askUserTool{} }

// Definition returns the model-facing schema. Description is the
// LLM's only hint for when to use it: keep it conservative —
// "ask the human only when truly needed" — to discourage chatty
// models from interrupting on every minor uncertainty.
func (askUserTool) Definition() message.Definition {
	return message.DefineSchema(
		Name,
		"Ask the human user a clarifying question and "+
			"wait for their reply. Use only when you genuinely "+
			"cannot proceed without their input — most questions "+
			"can be answered from context. Returns the user's reply "+
			"as a string.",
		message.ToolProperty("prompt", "string", "The question to display to the user."),
	).Required("prompt").DisallowAdditionalProperties().Build()
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

	host, ok := agent.HostFromContext(ctx)
	if !ok || host == nil {
		// No host on ctx means the tool is running outside an
		// engine that wired one up (raw test path, batch run
		// with NoopHost). Surface NotAvailable so the LLM sees
		// "this is not currently a supported capability" rather
		// than crashing or returning nonsense.
		return "", errdefs.NotAvailablef("ask_user: no agent.Host on ctx; did the engine wire it via agent.ContextWithHost?")
	}
	prompt := agent.UserPrompt{
		Parts:  []message.Part{message.TextPart{Text: a.Prompt}},
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
func replyText(r agent.UserReply) string {
	var b strings.Builder
	wrote := false
	for _, p := range r.Parts {
		if wrote {
			b.WriteByte('\n')
		}
		// message.Part is a sealed interface; switch on Kind().
		switch p.Kind() {
		case message.PartText:
			if tp, ok := p.(message.TextPart); ok {
				b.WriteString(tp.Text)
			}
		default:
			// Non-text part: write a minimal marker. We
			// deliberately avoid base64-blobbing media into
			// the model context — that would balloon token
			// counts for no immediate gain.
			b.WriteString("[user attached a non-text part: ")
			b.WriteString(string(p.Kind()))
			b.WriteString("]")
		}
		wrote = true
	}
	return b.String()
}

// Compile-time assertion the tool satisfies the contract. Keeps
// signature drift in sdk/tool from silently breaking the tool.
var _ tool.Tool = askUserTool{}
