package hook

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// RecallPreparerKind is the type name that identifies a
// memory.recall factory in a deploy document.
const RecallPreparerKind = "memory.recall"

// RecallSettings is the deploy-side settings for the
// memory.recall Preparer.
type RecallSettings struct {
	// Scope is the memory scope the Recall runs against.
	// Empty fields fall back to runtime.Spec().DefaultScope.
	Scope ScopeConfig `yaml:"scope"`
	// Conversation is the transcript key the query
	// optionally scopes to. Empty means "all long-term
	// memory under this scope".
	Conversation string `yaml:"conversation,omitempty"`
	// Query selects either a literal search string or a Board
	// variable containing one. Exactly one field is required.
	Query QuerySpec `yaml:"query"`
	// TopK caps the number of hits returned. Falls back
	// to Spec.DefaultTopK / Spec.FallbackTopK when 0.
	TopK int `yaml:"top_k,omitempty"`
	// Filters carries opaque key/value hints (e.g.
	// dataset=knowledge_base) that the impl applies
	// during scoring.
	Filters map[string]string `yaml:"filters,omitempty"`
	// MinScore filters low-relevance hits at the kernel
	// level. Set per agent when relevance quality is
	// measurable.
	MinScore float64 `yaml:"min_score,omitempty"`
	// Into is the board var name the hits are written to
	// as []memory.Hit. The prompt builder reads it.
	Into string `yaml:"into"`
}

// QuerySpec selects the source of a recall query. Exactly one of Literal or
// Board must be non-empty.
type QuerySpec struct {
	Literal string `yaml:"literal,omitempty"`
	Board   string `yaml:"board,omitempty"`
}

// NewRecallPreparerFactory returns the deploy factory for
// the memory.recall Preparer.
func NewRecallPreparerFactory() deploy.PreparerFactory {
	return buildRecallPreparer
}

func buildRecallPreparer(ctx context.Context, in deploy.HookInput) (agent.Preparer, error) {
	settings, err := deploy.DecodeSettings[RecallSettings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory hook: decode %s settings: %w", RecallPreparerKind, err))
	}
	if settings.Into == "" {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory hook: %s requires settings.into (board var name)",
			RecallPreparerKind))
	}
	if err := settings.Query.validate(); err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory hook: %s settings.query: %w", RecallPreparerKind, err))
	}
	rt, err := resolveRuntime(in)
	if err != nil {
		return nil, err
	}
	scope := resolveScope(rt, settings.Scope)
	return agent.PreparerFunc(func(ctx context.Context, _ agent.Identity, agentReq *agent.Request, prev *agent.Board) (*agent.Board, error) {
		query, err := resolveRecallQuery(settings.Query, prev)
		if err != nil {
			return nil, err
		}
		conversationID := settings.Conversation
		if conversationID == "" && agentReq != nil {
			conversationID = agentReq.ContextID
		}
		req := memory.RecallRequest{
			Scope:          scope,
			ConversationID: conversationID,
			Query:          query,
			TopK:           settings.TopK,
			Filters:        settings.Filters,
			MinScore:       settings.MinScore,
		}
		resp, err := rt.ExecuteRecall(ctx, req)
		if err != nil {
			return nil, err
		}
		next := prev.Clone()
		next.SetVar(settings.Into, resp.Hits)
		return next, nil
	}), nil
}

func (q QuerySpec) validate() error {
	if (q.Literal == "") == (q.Board == "") {
		return fmt.Errorf("exactly one of literal or board must be non-empty")
	}
	return nil
}

func resolveRecallQuery(configured QuerySpec, board *agent.Board) (string, error) {
	if configured.Literal != "" {
		return configured.Literal, nil
	}
	if board == nil {
		return "", errdefs.Validation(fmt.Errorf(
			"memory hook: %s query board var %q is missing",
			RecallPreparerKind, configured.Board))
	}
	value, ok := board.GetVar(configured.Board)
	if !ok {
		return "", errdefs.Validation(fmt.Errorf(
			"memory hook: %s query board var %q is missing",
			RecallPreparerKind, configured.Board))
	}
	query, ok := value.(string)
	if !ok {
		return "", errdefs.Validation(fmt.Errorf(
			"memory hook: %s query board var %q must be a string (got %T)",
			RecallPreparerKind, configured.Board, value))
	}
	return query, nil
}
