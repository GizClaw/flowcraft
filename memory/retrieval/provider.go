// Package retrieval composes fusion, hydration, and deterministic packing.
package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/memory/retrieval/hydrate"
	messagesource "github.com/GizClaw/flowcraft/memory/sources/message"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

type Diagnostic struct {
	Stage string
	Lane  string
	Err   error
}

type Provider struct {
	Fusion        *fusion.Fusion
	Messages      *messagesource.MessageStore
	Summary       component.Searcher
	Hydrator      hydrate.Hydrator
	Packer        component.Packer
	Recent        RecentConfig
	Reranker      RerankerConfig
	ExpandParents bool
	RecallEvents  RecallEventRecorder
	Visibility    Visibility
	Clock         func() time.Time

	mu          sync.RWMutex
	diagnostics []Diagnostic
}

// RecentConfig bounds the deterministic canonical-message lane independently
// before all lanes enter the final shared pack budget.
type RecentConfig struct {
	MaxItems  int
	MaxTokens int
}

type RerankerConfig struct {
	Enabled bool
	Value   component.Reranker
}

// ProviderConfig declares the fixed recent + hybrid + optional summary path.
type ProviderConfig struct {
	Fusion        *fusion.Fusion
	Messages      *messagesource.MessageStore
	Summary       component.Searcher
	Hydrator      hydrate.Hydrator
	Packer        component.Packer
	Recent        RecentConfig
	Reranker      RerankerConfig
	ExpandParents bool
	RecallEvents  RecallEventRecorder
	Visibility    Visibility
	Clock         func() time.Time
}

type RecallEventRecorder interface {
	RecordRecall(context.Context, sdkmemory.RecallEvent) error
}

type Visibility interface {
	Visible(context.Context, sdkmemory.Scope, string) (bool, error)
}

var _ sdkmemory.ContextProvider = (*Provider)(nil)

func NewProvider(fusor *fusion.Fusion, hydrator hydrate.Hydrator, packer component.Packer) (*Provider, error) {
	return NewProviderWithConfig(ProviderConfig{Fusion: fusor, Hydrator: hydrator, Packer: packer})
}

func NewProviderWithConfig(config ProviderConfig) (*Provider, error) {
	if config.Fusion == nil || config.Hydrator == nil || config.Packer == nil {
		return nil, errors.New("retrieval: fusion, hydrator, and packer are required")
	}
	if config.Recent.MaxItems < 0 || config.Recent.MaxTokens < 0 {
		return nil, errors.New("retrieval: recent limits must not be negative")
	}
	if config.Reranker.Enabled && config.Reranker.Value == nil {
		return nil, errors.New("retrieval: enabled reranker is required")
	}
	if config.Recent.MaxItems == 0 {
		config.Recent.MaxItems = 8
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Provider{
		Fusion: config.Fusion, Messages: config.Messages, Summary: config.Summary,
		Hydrator: config.Hydrator, Packer: config.Packer, Recent: config.Recent,
		Reranker:      config.Reranker,
		ExpandParents: config.ExpandParents,
		RecallEvents:  config.RecallEvents, Visibility: config.Visibility, Clock: config.Clock,
	}, nil
}

func (provider *Provider) Context(ctx context.Context, request sdkmemory.ContextRequest) (sdkmemory.ContextResult, error) {
	if provider == nil || provider.Fusion == nil || provider.Hydrator == nil || provider.Packer == nil {
		return sdkmemory.ContextResult{}, sdkmemory.NewError(sdkmemory.KindNotConfigured, "context", errors.New("retrieval provider is incomplete"))
	}
	if ctx == nil {
		return sdkmemory.ContextResult{}, sdkmemory.NewError(sdkmemory.KindInvalidRequest, "context", errors.New("context is required"))
	}
	if err := request.Validate(); err != nil {
		return sdkmemory.ContextResult{}, err
	}
	metadata := request.Metadata.Clone()
	if metadata == nil {
		metadata = sdkmemory.Metadata{}
	}
	if request.ConversationID != "" {
		metadata["conversation_id"] = request.ConversationID
	}
	if len(request.DatasetIDs) > 0 {
		encoded, _ := json.Marshal(request.DatasetIDs)
		metadata["dataset_ids"] = string(encoded)
	}
	limit := request.Budget.MaxItems
	if limit <= 0 {
		limit = 20
	}
	limit *= 3
	candidates, diagnostics, recentCount, err := provider.readCandidates(ctx, request, metadata, limit)
	if err != nil {
		provider.setDiagnostics(diagnostics)
		return sdkmemory.ContextResult{}, err
	}
	hydrated := make([]sdkmemory.ContextItem, 0, len(candidates))
	eligible := 0
	for index, candidate := range candidates {
		isRecent := index < recentCount
		if (!isRecent && candidate.Score < request.MinScore) || !matchesRequest(candidate, request) {
			continue
		}
		eligible++
		if err := ctx.Err(); err != nil {
			provider.setDiagnostics(diagnostics)
			return sdkmemory.ContextResult{}, sdkmemory.NewError(sdkmemory.KindOperationInterrupted, "context", err)
		}
		item, hydrateErr := provider.Hydrator.Hydrate(ctx, request.Scope, candidate)
		if hydrateErr != nil {
			diagnostics = append(diagnostics, Diagnostic{Stage: "hydrate", Lane: candidate.Lane, Err: hydrateErr})
			continue
		}
		if !isNilInterface(provider.Visibility) && !isRecent {
			visible, visibilityErr := provider.Visibility.Visible(ctx, request.Scope, item.Identity(request.Scope))
			if visibilityErr != nil {
				diagnostics = append(diagnostics, Diagnostic{Stage: "visibility", Lane: candidate.Lane, Err: visibilityErr})
				continue
			}
			if !visible {
				eligible--
				continue
			}
		}
		if isRecent {
			item.SourceClass = sdkmemory.ContextSourceRecent
		} else if candidate.Lane == "summary" {
			item.SourceClass = sdkmemory.ContextSourceSummary
		} else {
			item.SourceClass = sdkmemory.ContextSourceLongTerm
		}
		hydrated = append(hydrated, item)
		if provider.ExpandParents && !isRecent {
			if progressive, ok := provider.Hydrator.(hydrate.Progressive); ok {
				current := item
				for current.ParentID != "" {
					parent, found, parentErr := progressive.Parent(ctx, request.Scope, current)
					if parentErr != nil {
						diagnostics = append(diagnostics, Diagnostic{Stage: "hydrate_parent", Lane: candidate.Lane, Err: parentErr})
						break
					}
					if !found {
						break
					}
					parent.SourceClass = item.SourceClass
					hydrated = append(hydrated, parent)
					current = parent
				}
			}
		}
	}
	if eligible > 0 && len(hydrated) == 0 {
		provider.setDiagnostics(diagnostics)
		return sdkmemory.ContextResult{}, sdkmemory.NewError(
			sdkmemory.KindProviderFailure, "context", errors.New("retrieval: all eligible candidates failed hydration"),
		)
	}
	result, err := provider.Packer.Pack(ctx, hydrated, request.Budget)
	if err != nil {
		diagnostics = append(diagnostics, Diagnostic{Stage: "pack", Err: err})
		provider.setDiagnostics(diagnostics)
		if ctx.Err() != nil {
			return sdkmemory.ContextResult{}, sdkmemory.NewError(sdkmemory.KindOperationInterrupted, "context", ctx.Err())
		}
		return sdkmemory.ContextResult{}, sdkmemory.NewError(sdkmemory.KindInternal, "context", fmt.Errorf("retrieval: pack: %w", err))
	}
	result.RecallEventID = request.RecallEventID
	if request.RecallEventID != "" && !isNilInterface(provider.RecallEvents) {
		scores := make(map[string]float64)
		for _, item := range result.Items {
			if item.SourceClass != sdkmemory.ContextSourceLongTerm && item.SourceClass != sdkmemory.ContextSourceSummary {
				continue
			}
			identity := item.Identity(request.Scope)
			if current, exists := scores[identity]; !exists || item.Score > current {
				scores[identity] = item.Score
			}
		}
		if len(scores) > 0 {
			ids := make([]string, 0, len(scores))
			for id := range scores {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			values := make([]float64, len(ids))
			for index, id := range ids {
				values[index] = scores[id]
			}
			event := sdkmemory.RecallEvent{ID: request.RecallEventID, Scope: request.Scope, ItemIDs: ids, Scores: values, Time: provider.Clock().UTC()}
			if recordErr := provider.RecallEvents.RecordRecall(ctx, event); recordErr != nil {
				diagnostics = append(diagnostics, Diagnostic{Stage: "reinforce", Err: recordErr})
				provider.setDiagnostics(diagnostics)
				return sdkmemory.ContextResult{}, sdkmemory.NewError(sdkmemory.KindProviderFailure, "context", fmt.Errorf("retrieval: reinforce: %w", recordErr))
			}
		}
	}
	provider.setDiagnostics(diagnostics)
	return result, nil
}

func (provider *Provider) readCandidates(ctx context.Context, request sdkmemory.ContextRequest, metadata sdkmemory.Metadata, limit int) ([]component.Candidate, []Diagnostic, int, error) {
	recent, err := provider.recentCandidates(ctx, request)
	if err != nil {
		return nil, nil, 0, sdkmemory.NewError(sdkmemory.KindProviderFailure, "context", fmt.Errorf("retrieval: recent: %w", err))
	}
	candidates := append([]component.Candidate(nil), recent...)
	diagnostics := make([]Diagnostic, 0)
	if strings.TrimSpace(request.Query) != "" {
		fused, searchErr := provider.Fusion.SearchDetailed(ctx, component.SearchRequest{
			Scope: request.Scope, Query: request.Query, Limit: limit, Metadata: metadata,
		})
		for _, diagnostic := range fused.Diagnostics {
			diagnostics = append(diagnostics, Diagnostic{Stage: "search", Lane: diagnostic.Lane, Err: diagnostic.Err})
		}
		if searchErr != nil {
			if len(recent) == 0 {
				return nil, diagnostics, 0, searchErr
			}
		} else {
			hybrid := fused.Candidates
			if provider.Reranker.Enabled && len(hybrid) > 0 {
				reranked, rerankErr := provider.Reranker.Value.Rerank(ctx, component.RerankRequest{
					Scope: request.Scope, Query: request.Query, Candidates: cloneCandidates(hybrid),
				})
				if rerankErr != nil {
					if ctx.Err() != nil {
						return nil, diagnostics, len(recent), sdkmemory.NewError(
							sdkmemory.KindOperationInterrupted, "context", ctx.Err(),
						)
					}
					diagnostics = append(diagnostics, Diagnostic{Stage: "rerank", Err: rerankErr})
				} else if normalized, validationErr := normalizeReranked(hybrid, reranked); validationErr != nil {
					diagnostics = append(diagnostics, Diagnostic{Stage: "rerank", Err: validationErr})
				} else {
					hybrid = normalized
				}
			}
			candidates = append(candidates, hybrid...)
		}
		if provider.Summary != nil {
			summary, summaryErr := provider.Summary.Search(ctx, component.SearchRequest{
				Scope: request.Scope, Query: request.Query, Limit: limit, Metadata: metadata,
			})
			if summaryErr != nil {
				diagnostics = append(diagnostics, Diagnostic{Stage: "search", Lane: "summary", Err: summaryErr})
			} else {
				for index := range summary {
					summary[index].Lane = "summary"
				}
				candidates = append(candidates, summary...)
			}
		}
	}
	return candidates, diagnostics, len(recent), nil
}

func normalizeReranked(input, output []component.Candidate) ([]component.Candidate, error) {
	allowed := make(map[string]component.Candidate, len(input))
	for _, candidate := range input {
		allowed[candidateIdentity(candidate)] = candidate
	}
	seen := make(map[string]struct{}, len(output))
	for _, candidate := range output {
		if err := candidate.Validate(); err != nil {
			return nil, fmt.Errorf("retrieval: invalid reranked candidate: %w", err)
		}
		if candidate.Score < 0 || candidate.Score > 1 {
			return nil, errors.New("retrieval: reranker score must be in [0,1]")
		}
		key := candidateIdentity(candidate)
		original, ok := allowed[key]
		returnedScore := candidate.Score
		candidate.Score = original.Score
		if !ok || candidate.ID != original.ID || candidate.Address != original.Address ||
			candidate.Source != original.Source || !reflect.DeepEqual(candidate, original) {
			return nil, errors.New("retrieval: reranker injected or moved a candidate")
		}
		candidate.Score = returnedScore
		if _, duplicate := seen[key]; duplicate {
			return nil, errors.New("retrieval: reranker returned duplicate candidate")
		}
		seen[key] = struct{}{}
	}
	normalized := make([]component.Candidate, len(output))
	for index, candidate := range output {
		normalized[index] = allowed[candidateIdentity(candidate)].Clone()
		normalized[index].Score = candidate.Score
	}
	return normalized, nil
}

func candidateIdentity(candidate component.Candidate) string {
	address := candidate.Address
	if !address.IsZero() {
		return strings.Join([]string{
			string(address.Kind), address.ConversationID, address.DatasetID, address.DocumentID, address.ItemID,
		}, "\x00")
	}
	return candidate.ID
}

// isNilInterface reports whether an interface value is nil or holds a typed
// nil pointer, which would otherwise panic on method dispatch.
func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func cloneCandidates(values []component.Candidate) []component.Candidate {
	result := make([]component.Candidate, len(values))
	for index, candidate := range values {
		result[index] = candidate.Clone()
	}
	return result
}

func (provider *Provider) recentCandidates(ctx context.Context, request sdkmemory.ContextRequest) ([]component.Candidate, error) {
	if provider.Messages == nil || request.ConversationID == "" {
		return []component.Candidate{}, nil
	}
	maxItems := provider.Recent.MaxItems
	if request.RecentLimit > 0 {
		maxItems = request.RecentLimit
	}
	if request.Budget.MaxItems > 0 && maxItems > request.Budget.MaxItems {
		maxItems = request.Budget.MaxItems
	}
	records, err := provider.Messages.Latest(ctx, request.Scope, request.ConversationID, messagesource.LatestOptions{Limit: maxItems})
	if err != nil {
		return nil, err
	}
	maxTokens := provider.Recent.MaxTokens
	if request.RecentMaxTokens > 0 {
		maxTokens = request.RecentMaxTokens
	}
	if maxTokens > 0 {
		used := 0
		start := len(records)
		for start > 0 {
			count := max(1, (len([]rune(records[start-1].Message.Content.Text()))+3)/4)
			if used+count > maxTokens {
				break
			}
			used += count
			start--
		}
		records = records[start:]
	}
	result := make([]component.Candidate, len(records))
	for index, record := range records {
		result[index] = component.Candidate{
			ID: record.ID, Lane: "recent", Name: "message", Score: 1,
			Source: sdkmemory.SourceRef{
				Kind: sdkmemory.SourceMessage, ID: record.ConversationID + "/" + record.ID,
				Revision: strconv.FormatUint(record.Seq, 10),
			},
			Address: component.CandidateAddress{
				Kind: sdkmemory.ContextRawMessage, ConversationID: record.ConversationID, ItemID: record.ID,
			},
		}
	}
	return result, nil
}

// LastDiagnostics returns a copied internal diagnostic snapshot. The SDK result
// intentionally remains clean because it has no diagnostics field.
func (provider *Provider) LastDiagnostics() []Diagnostic {
	if provider == nil {
		return nil
	}
	provider.mu.RLock()
	defer provider.mu.RUnlock()
	return append([]Diagnostic(nil), provider.diagnostics...)
}

func (provider *Provider) setDiagnostics(values []Diagnostic) {
	provider.mu.Lock()
	provider.diagnostics = append([]Diagnostic(nil), values...)
	provider.mu.Unlock()
}

func matchesRequest(candidate component.Candidate, request sdkmemory.ContextRequest) bool {
	if request.ConversationID != "" && candidate.Address.ConversationID != "" &&
		candidate.Address.ConversationID != request.ConversationID {
		return false
	}
	if len(request.DatasetIDs) == 0 || candidate.Address.DatasetID == "" {
		return true
	}
	for _, datasetID := range request.DatasetIDs {
		if candidate.Address.DatasetID == datasetID {
			return true
		}
	}
	return false
}
