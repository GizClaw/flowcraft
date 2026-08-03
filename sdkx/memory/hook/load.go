package hook

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// LoadPreparerKind is the type name that identifies a
// memory.load factory in a deploy document.
const LoadPreparerKind = "memory.load"

// LoadSettings is the deploy-side settings for the
// memory.load Preparer. The shape is documented; the factory
// decodes it strictly with deploy.DecodeSettings.
type LoadSettings struct {
	// Scope is the memory scope the Load runs against.
	// Empty fields fall back to runtime.Spec().DefaultScope.
	Scope ScopeConfig `yaml:"scope"`
	// Conversation is the transcript key. Empty means the
	// factory lets the runtime infer a default — most
	// deployments supply it explicitly.
	Conversation string `yaml:"conversation,omitempty"`
	// Limit caps the number of records returned. The
	// runtime falls back to Spec.DefaultLoadLimit /
	// Spec.FallbackLoadLimit when this is 0; the hook
	// should usually set it explicitly so behaviour is
	// obvious in the document.
	Limit int `yaml:"limit,omitempty"`
	// Into is the board channel name the loaded records
	// are written to. The factory converts each
	// memory.Record to inference.Message and appends to
	// the named channel; the engine then reads it as a
	// normal channel.
	Into string `yaml:"into"`
}

// NewLoadPreparerFactory returns the deploy factory for the
// memory.load Preparer. Register it on a deploy.Builder under
// the LoadPreparerKind type name.
func NewLoadPreparerFactory() deploy.PreparerFactory {
	return buildLoadPreparer
}

func buildLoadPreparer(ctx context.Context, in deploy.HookInput) (agent.Preparer, error) {
	settings, err := deploy.DecodeSettings[LoadSettings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory hook: decode %s settings: %w", LoadPreparerKind, err))
	}
	if settings.Into == "" {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory hook: %s requires settings.into (board channel name)",
			LoadPreparerKind))
	}
	rt, err := resolveRuntime(in)
	if err != nil {
		return nil, err
	}
	scope := resolveScope(rt, settings.Scope)
	return agent.PreparerFunc(func(ctx context.Context, _ agent.Identity, agentReq *agent.Request, prev *agent.Board) (*agent.Board, error) {
		conversationID := settings.Conversation
		if conversationID == "" && agentReq != nil {
			conversationID = agentReq.ContextID
		}
		if conversationID == "" {
			return prev, nil
		}
		req := memory.LoadRequest{
			Scope:          scope,
			ConversationID: conversationID,
			Limit:          settings.Limit,
		}
		resp, err := rt.ExecuteLoad(ctx, req)
		if err != nil {
			return nil, err
		}
		// Convert records to messages and append to the
		// configured channel. Clone the previous board so
		// engine and downstream links can rely on
		// immutability, per agent.Preparer contract.
		next := prev.Clone()
		for _, rec := range resp.Records {
			next.AppendChannelMessage(settings.Into, rec.Message)
		}
		return next, nil
	}), nil
}
