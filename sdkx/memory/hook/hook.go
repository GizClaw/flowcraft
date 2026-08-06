// Package hook adapts memory capabilities to agent lifecycle hooks.
package hook

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
	memoryrender "github.com/GizClaw/flowcraft/sdkx/memory/render"
)

const (
	ContextType = "memory.context"
	TurnType    = "memory.turn"
	depName     = "memory"
)

type ScopeSettings struct {
	RuntimeID string `json:"runtime_id"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

func (s ScopeSettings) scope() sdkmemory.Scope {
	return sdkmemory.Scope{RuntimeID: s.RuntimeID, UserID: s.UserID, AgentID: s.AgentID}
}

type QuerySettings struct {
	Literal        string `json:"literal,omitempty"`
	Board          string `json:"board,omitempty"`
	CurrentMessage bool   `json:"current_message,omitempty"`
	RecentOnly     bool   `json:"recent_only,omitempty"`
}

type BudgetSettings struct {
	MaxTokens int `json:"max_tokens,omitempty"`
	MaxItems  int `json:"max_items,omitempty"`
	MaxChars  int `json:"max_chars,omitempty"`
}

func (b BudgetSettings) budget() sdkmemory.Budget {
	return sdkmemory.Budget{MaxTokens: b.MaxTokens, MaxItems: b.MaxItems, MaxChars: b.MaxChars}
}

type ContextSettings struct {
	Query          QuerySettings   `json:"query"`
	Scope          ScopeSettings   `json:"scope"`
	ConversationID string          `json:"conversation_id,omitempty"`
	DatasetIDs     []string        `json:"dataset_ids,omitempty"`
	Budget         BudgetSettings  `json:"budget,omitempty"`
	MinScore       float64         `json:"min_score,omitempty"`
	Output         string          `json:"output"`
	Render         *RenderSettings `json:"render,omitempty"`
}

// RenderSettings selects exactly one configured ContextRenderer. GoTemplate
// is currently the only YAML renderer; the union remains explicit so adding a
// structured renderer later does not change existing configuration semantics.
type RenderSettings struct {
	Output     string                           `json:"output"`
	GoTemplate *memoryrender.GoTemplateSettings `json:"gotmpl,omitempty"`
}

type TurnSettings struct {
	Scope          ScopeSettings `json:"scope"`
	ConversationID string        `json:"conversation_id,omitempty"`
	Channel        string        `json:"channel,omitempty"`
}

// ContextPreparerFactory constructs memory.context.
func ContextPreparerFactory(_ context.Context, input sdkconfig.Input) (agent.Preparer, error) {
	var settings ContextSettings
	if err := input.Settings.Decode(&settings); err != nil {
		return nil, errdefs.Validation(fmt.Errorf("%s: decode settings: %w", ContextType, err))
	}
	if err := settings.validate(); err != nil {
		return nil, errdefs.Validation(err)
	}
	renderer, err := settings.contextRenderer()
	if err != nil {
		return nil, errdefs.Validation(err)
	}
	provider, err := resolveContext(input)
	if err != nil {
		return nil, err
	}
	return agent.PreparerFunc(func(ctx context.Context, identity agent.Identity, req *agent.Request, previous *agent.Board) (*agent.Board, error) {
		if req == nil || previous == nil {
			return nil, errdefs.Validationf("%s: request and previous board are required", ContextType)
		}
		query := settings.Query.Literal
		if settings.Query.Board != "" {
			query = previous.GetVarString(settings.Query.Board)
		} else if settings.Query.CurrentMessage {
			query = req.Message.Content.Text()
		}
		conversationID := settings.ConversationID
		if conversationID == "" {
			conversationID = req.ContextID
		}
		if strings.TrimSpace(query) == "" && strings.TrimSpace(conversationID) == "" {
			return nil, errdefs.Validationf("%s: resolved query and conversation ID are empty", ContextType)
		}
		result, err := provider.Context(ctx, sdkmemory.ContextRequest{
			Scope: settings.Scope.scope(), ConversationID: conversationID,
			DatasetIDs: append([]string(nil), settings.DatasetIDs...), Query: query,
			Budget: settings.Budget.budget(), MinScore: settings.MinScore,
			RecallEventID: identity.RunID,
		})
		if err != nil {
			return nil, err
		}
		var rendered message.Content
		if renderer != nil {
			rendered, err = renderer.Render(ctx, result)
			if err != nil {
				if ctx.Err() != nil {
					return nil, errdefs.Interrupted(ctx.Err())
				}
				return nil, errdefs.Internal(fmt.Errorf("%s: render context: %w", ContextType, err))
			}
		}
		board := previous.Clone()
		board.SetVar(settings.Output, cloneItems(result.Items))
		if settings.Render != nil {
			board.SetVar(settings.Render.Output, rendered.Clone())
		}
		return board, nil
	}), nil
}

// TurnCommitterFactory constructs memory.turn.
func TurnCommitterFactory(_ context.Context, input sdkconfig.Input) (agent.Committer, error) {
	var settings TurnSettings
	if err := input.Settings.Decode(&settings); err != nil {
		return nil, errdefs.Validation(fmt.Errorf("%s: decode settings: %w", TurnType, err))
	}
	if settings.Channel == "" {
		settings.Channel = agent.MainChannel
	}
	if err := settings.validate(); err != nil {
		return nil, errdefs.Validation(err)
	}
	sink, err := resolveTurn(input)
	if err != nil {
		return nil, err
	}
	return agent.CommitterFunc(func(ctx context.Context, identity agent.Identity, req *agent.Request, result *agent.Result) error {
		if req == nil || result == nil {
			return errdefs.Validationf("%s: request and result are required", TurnType)
		}
		if result.LastBoard == nil {
			return errdefs.Validationf("%s: result last board is required", TurnType)
		}
		messages := result.LastBoard.Channel(settings.Channel)
		if len(messages) == 0 {
			return nil
		}
		conversationID := settings.ConversationID
		if conversationID == "" {
			conversationID = req.ContextID
		}
		return sink.CommitTurn(ctx, sdkmemory.Turn{
			Scope: settings.Scope.scope(), ConversationID: conversationID,
			IdempotencyKey: identity.RunID, Messages: messages,
		})
	}), nil
}

func resolveAssembly(input sdkconfig.Input) (sdkmemory.Assembly, error) {
	raw, ok := input.Dep(depName)
	if !ok {
		return nil, errdefs.NotFoundf("memory hook: dependency %q is not bound", depName)
	}
	assembly, ok := raw.(sdkmemory.Assembly)
	if !ok || isNilAssembly(assembly) {
		return nil, errdefs.Validationf(
			"memory hook: dependency %q has Go type %T, want memory.Assembly",
			depName, raw,
		)
	}
	return assembly, nil
}

func resolveContext(input sdkconfig.Input) (sdkmemory.ContextProvider, error) {
	assembly, err := resolveAssembly(input)
	if err != nil {
		return nil, err
	}
	return assembly, nil
}

func resolveTurn(input sdkconfig.Input) (sdkmemory.TurnSink, error) {
	assembly, err := resolveAssembly(input)
	if err != nil {
		return nil, err
	}
	return assembly, nil
}

func isNilAssembly(assembly sdkmemory.Assembly) bool {
	if assembly == nil {
		return true
	}
	value := reflect.ValueOf(assembly)
	return value.Kind() == reflect.Ptr && value.IsNil()
}

func (s ContextSettings) validate() error {
	if err := s.Scope.scope().Validate(); err != nil {
		return fmt.Errorf("%s: scope: %w", ContextType, err)
	}
	selected := 0
	if strings.TrimSpace(s.Query.Literal) != "" {
		selected++
	}
	if strings.TrimSpace(s.Query.Board) != "" {
		selected++
	}
	if s.Query.CurrentMessage {
		selected++
	}
	if s.Query.RecentOnly {
		selected++
	}
	if selected != 1 {
		return fmt.Errorf("%s: query must select exactly one of literal, board, current_message, or recent_only", ContextType)
	}
	if strings.TrimSpace(s.Output) == "" || strings.HasPrefix(s.Output, "__") {
		return fmt.Errorf("%s: output must be a non-reserved board variable", ContextType)
	}
	if s.Render != nil {
		if strings.TrimSpace(s.Render.Output) == "" || strings.HasPrefix(s.Render.Output, "__") {
			return fmt.Errorf("%s: render output must be a non-reserved board variable", ContextType)
		}
		if s.Render.Output == s.Output {
			return fmt.Errorf("%s: render output must differ from typed output", ContextType)
		}
		if s.Render.GoTemplate == nil {
			return fmt.Errorf("%s: render must select exactly one renderer", ContextType)
		}
	}
	if err := s.Budget.budget().Validate(); err != nil {
		return fmt.Errorf("%s: budget: %w", ContextType, err)
	}
	if math.IsNaN(s.MinScore) || math.IsInf(s.MinScore, 0) || s.MinScore < 0 || s.MinScore > 1 {
		return fmt.Errorf("%s: min_score must be in [0,1]", ContextType)
	}
	seen := make(map[string]struct{}, len(s.DatasetIDs))
	for i, id := range s.DatasetIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("%s: dataset_ids[%d] is empty", ContextType, i)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("%s: duplicate dataset id %q", ContextType, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (s ContextSettings) contextRenderer() (sdkmemory.ContextRenderer, error) {
	if s.Render == nil {
		return nil, nil
	}
	renderer, err := memoryrender.NewGoTemplate(*s.Render.GoTemplate)
	if err != nil {
		return nil, fmt.Errorf("%s: gotmpl: %w", ContextType, err)
	}
	return renderer, nil
}

func (s TurnSettings) validate() error {
	if err := s.Scope.scope().Validate(); err != nil {
		return fmt.Errorf("%s: scope: %w", TurnType, err)
	}
	if strings.TrimSpace(s.Channel) == "" {
		return fmt.Errorf("%s: channel is required", TurnType)
	}
	return nil
}

func cloneItems(items []sdkmemory.ContextItem) []sdkmemory.ContextItem {
	cloned := make([]sdkmemory.ContextItem, len(items))
	for i, item := range items {
		cloned[i] = item.Clone()
	}
	return cloned
}
