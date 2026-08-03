package hook

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// AppendCommitterKind is the type name that identifies a
// memory.append factory in a deploy document.
const AppendCommitterKind = "memory.append"

// AppendSettings is the deploy-side settings for the
// memory.append Committer. The factory reads the named
// channel off the result board and writes the messages
// inside to the transcript.
type AppendSettings struct {
	// Scope is the memory scope the Append writes against.
	// Empty fields fall back to runtime.Spec().DefaultScope.
	Scope ScopeConfig `yaml:"scope"`
	// Conversation is the transcript key. Empty means the
	// runtime picks a default; most deployments supply it
	// explicitly.
	Conversation string `yaml:"conversation,omitempty"`
	// Channel is the board channel name the new messages
	// are read from. The factory reads every message on
	// the channel at commit time and writes them as
	// records. The canonical use is the agent.MainChannel
	// — i.e. the turn's actual assistant output plus any
	// earlier user message already on the board.
	Channel string `yaml:"channel"`
	// Metadata is opaque key/value annotations the
	// factory passes through to AppendRequest.Metadata.
	// Useful for tagging records with the run id, the
	// agent id, or a request id, depending on the impl.
	Metadata map[string]string `yaml:"metadata,omitempty"`
}

// NewAppendCommitterFactory returns the deploy factory for
// the memory.append Committer.
func NewAppendCommitterFactory() deploy.CommitterFactory {
	return buildAppendCommitter
}

func buildAppendCommitter(ctx context.Context, in deploy.HookInput) (agent.Committer, error) {
	settings, err := deploy.DecodeSettings[AppendSettings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory hook: decode %s settings: %w", AppendCommitterKind, err))
	}
	if settings.Channel == "" {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory hook: %s requires settings.channel (board channel name to read from)",
			AppendCommitterKind))
	}
	rt, err := resolveRuntime(in)
	if err != nil {
		return nil, err
	}
	scope := resolveScope(rt, settings.Scope)
	channel := settings.Channel
	conversation := settings.Conversation
	baseMetadata := settings.Metadata

	return agent.CommitterFunc(func(ctx context.Context, id agent.Identity, agentReq *agent.Request, res *agent.Result) error {
		// The Committer contract says we MUST NOT mutate
		// req / res. We only read the channel.
		msgs := res.LastBoard.Channel(channel)
		if len(msgs) == 0 {
			// Nothing to persist. Returning nil matches
			// the documented "durable finalizer" semantic:
			// a Committer that observes zero messages is
			// a no-op success.
			return nil
		}
		conversationID := conversation
		if conversationID == "" && agentReq != nil {
			conversationID = agentReq.ContextID
		}
		if conversationID == "" {
			return nil
		}
		records := make([]memory.Record, len(msgs))
		for i, m := range msgs {
			// Leave ID empty: the kernel back-fills it
			// with a unique value before persisting.
			records[i] = memory.Record{Message: m}
		}
		req := memory.AppendRequest{
			Scope:          scope,
			ConversationID: conversationID,
			// Identity.RunID is the canonical idempotency
			// key per the agent.Committer contract: a
			// retry after an ambiguous transport failure
			// re-runs with the same key, and the runtime
			// dedups the second write against the first.
			IdempotencyKey: id.RunID,
			Records:        records,
			Metadata:       cloneMetadata(baseMetadata),
		}
		// Carry the agent run id through the metadata so
		// observability hooks / debug tools can correlate
		// transcript records with runs without re-deriving
		// the id from the IdempotencyKey.
		if id.RunID != "" {
			req.Metadata["run_id"] = id.RunID
		}

		if _, err := rt.ExecuteAppend(ctx, req); err != nil {
			return err
		}
		return nil
	}), nil
}

func cloneMetadata(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src)+1)
	for key, value := range src {
		dst[key] = value
	}
	return dst
}
