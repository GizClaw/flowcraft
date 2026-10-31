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

	"github.com/GizClaw/flowcraft/backends/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
	"github.com/GizClaw/flowcraft/backends/memory/profile"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/pack"
	messagesource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

type Diagnostic struct {
	Stage string
	Lane  string
	Err   error
}

// maxParentDepth bounds chunk-hierarchy expansion. The document view also
// rejects cyclic parent chains, so this is a second line of defense against
// malformed or adversarial hierarchies.
const maxParentDepth = 32

// maxQuoteTexts bounds the process-lifetime source-quote cache. Reaching the
// bound clears the map: the entries are pure cache, so a cleared map costs one
// point read per source on the next request instead of unbounded growth.
const maxQuoteTexts = 4096

type Provider struct {
	Fusion        *fusion.Fusion
	Messages      *messagesource.MessageStore
	Hydrator      hydrate.Hydrator
	Packer        component.Packer
	Recent        RecentConfig
	ItemReranker  ItemReranker
	ScoreAdjuster ScoreAdjuster
	// Generations labels a search with the generation the request reads under,
	// so a lane that publishes its own generation can be told that its view
	// belongs to a replaced policy.
	Generations GenerationResolver
	// SourceQuotes is how many source messages one derived fact may pull into
	// the context (0 disables the expansion). A fact is a paraphrase, so the
	// wording the conversation actually used -- a title, a quoted number, a
	// list of activities -- lives in the source turn, not in the fact.
	SourceQuotes int
	// ObservedAtAnchor renders each raw message's observation anchor as a text
	// prefix in the evidence the answer model reads -- folded source quotes and
	// hydrated raw messages (memory.Settings.ObservedAtAnchor).
	ObservedAtAnchor bool
	ExpandParents    bool
	RecallEvents     RecallEventRecorder
	Visibility       Visibility
	Clock            func() time.Time
	// Profiles, when set, is the resident profile block this provider serves:
	// one bounded item per request, read by conversation rather than
	// retrieved, added after the lanes and read by the packer as its own
	// class. It is what makes stable material present on a request whose
	// words do not resemble it, and it is wired only when a deployment wants
	// every request to carry it.
	Profiles ProfileSource
	// ObservePool, when set, receives the hydrated items one request hands to
	// the packer -- the pool the budget chooses from and cuts. It exists for
	// diagnostics: a harness that only sees the pack cannot tell "the budget
	// cut this" from "retrieval never returned this". Production leaves it
	// nil, and the observer must neither mutate the slice nor keep it.
	ObservePool func(corememory.ContextRequest, []corememory.ContextItem)

	mu          sync.RWMutex
	diagnostics []Diagnostic
	// quoteTexts caches canonical message texts by hard partition key plus
	// "conversation/message" id. Messages are immutable, so a cached text can
	// never go stale, and folding source quotes used to issue one serial point
	// read per source per request (≈60 reads for a 20-fact context) against a
	// store that serialises every read behind one mutex.
	//
	// The scope is part of the key because message ids are per-stream sequence
	// numbers ("msg-…0001"): two scopes that use the same conversation id name
	// the same source id, and the cache must not hand one scope the other's
	// text. The map is bounded by maxQuoteTexts; message text is a cache, not
	// state, so dropping it costs one store read and never correctness.
	quoteMu    sync.Mutex
	quoteTexts map[string]string
	// stage totals attribute latency inside Context() even when requests run
	// concurrently (the per-request diagnostics snapshot is last-wins, which is
	// useless for attribution).
	stageMu     sync.Mutex
	stageTotals map[string]StageStat
}

// StageStat is the cumulative time and request count of one Context phase.
type StageStat struct {
	Total time.Duration
	Count int
}

// StageTotals reports cumulative per-phase latency since the provider started.
func (provider *Provider) StageTotals() map[string]StageStat {
	if provider == nil {
		return nil
	}
	provider.stageMu.Lock()
	defer provider.stageMu.Unlock()
	out := make(map[string]StageStat, len(provider.stageTotals))
	for stage, stat := range provider.stageTotals {
		out[stage] = stat
	}
	return out
}

// recordStage adds one phase measurement.
func (provider *Provider) recordStage(stage string, started time.Time) {
	provider.stageMu.Lock()
	if provider.stageTotals == nil {
		provider.stageTotals = make(map[string]StageStat)
	}
	stat := provider.stageTotals[stage]
	stat.Total += time.Since(started)
	stat.Count++
	provider.stageTotals[stage] = stat
	provider.stageMu.Unlock()
}

// RecentConfig bounds the deterministic canonical-message lane independently
// before all lanes enter the final shared pack budget.
type RecentConfig struct {
	MaxItems  int
	MaxTokens int
}

// ItemReranker reorders hydrated context items before packing. Unlike the
// candidate reranker it sees the full content, so implementations may call a
// model; returning fewer items is allowed. A failing reranker degrades to the
// pre-rerank order.
type ItemReranker interface {
	RerankItems(context.Context, string, []corememory.ContextItem) ([]corememory.ContextItem, error)
}

// ScoreAdjuster multiplies the retrieval score of hydrated items (for
// example decaying superseded facts). Implementations may also implement
// BulkScoreAdjuster to resolve a whole scope in one read.
type ScoreAdjuster interface {
	Adjust(context.Context, corememory.Scope, string) (float64, error)
}

// BulkScoreAdjuster resolves every adjustment for one scope in one call.
type BulkScoreAdjuster interface {
	ScoreOverlay(context.Context, corememory.Scope) (map[string]float64, error)
}

// ScopedScoreAdjuster resolves the adjustments of the named conversations in
// one call: the read a request needs when its candidates span a few
// conversations of a large scope. The provider prefers it over the whole-scope
// read, which stays the fallback for implementations that have no
// per-conversation read to offer.
type ScopedScoreAdjuster interface {
	ScoreOverlayFor(context.Context, corememory.Scope, []string) (map[string]float64, error)
}

// GenerationResolver reports the derivation generation readers should serve for
// one conversation. Lanes that store their own generation label -- the summary
// branch publishes one with its manifest -- drop candidates from a replaced
// generation instead of serving a view of a policy that no longer applies.
type GenerationResolver interface {
	ActiveGeneration(context.Context, corememory.Scope, string) (string, bool, error)
}

// ProviderConfig declares the fixed recent + hybrid + optional summary path.
type ProviderConfig struct {
	Fusion        *fusion.Fusion
	Messages      *messagesource.MessageStore
	Hydrator      hydrate.Hydrator
	Packer        component.Packer
	Recent        RecentConfig
	ItemReranker  ItemReranker
	ScoreAdjuster ScoreAdjuster
	// Generations labels a search with the generation the request reads under.
	// Without it a lane that cannot resolve a generation itself has nothing to
	// compare against, and a summary of a replaced policy stays visible.
	Generations GenerationResolver
	// SourceQuotes is how many source messages one derived fact may pull into
	// the context (0 disables the expansion). A fact is a paraphrase, so the
	// wording the conversation actually used -- a title, a quoted number, a
	// list of activities -- lives in the source turn, not in the fact.
	SourceQuotes int
	// ObservedAtAnchor renders each raw message's observation anchor as a text
	// prefix in the evidence the answer model reads (memory.Settings.ObservedAtAnchor).
	ObservedAtAnchor bool
	ExpandParents    bool
	RecallEvents     RecallEventRecorder
	Visibility       Visibility
	Clock            func() time.Time
	// Profiles, when set, is the resident profile block: memory about the
	// conversation's participants that is not retrieved because the request
	// looked like it, but served on every request that names the
	// conversation, up to the block's own bound. Nothing in the request
	// selects it -- not the query, the score floor or the dataset filters --
	// so a deployment wires it only when every request carrying it is what it
	// wants, and the packer takes its room before the retrieved classes fill
	// the budget.
	Profiles ProfileSource
}

type RecallEventRecorder interface {
	RecordRecall(context.Context, corememory.RecallEvent) error
}

type Visibility interface {
	Visible(context.Context, corememory.Scope, string) (bool, error)
}

// ProfileSource reads one conversation's resident profile block. The hide
// filter is the read path's visibility overlay as a question about facts: a
// distilled line names the facts a pass judged it from, and the block store
// asks whether any of them is hidden before it serves the line. It is how a
// hide reaches the block, which is a reading of the corpus rather than a copy
// of it.
type ProfileSource interface {
	BlockServing(context.Context, corememory.Scope, string, func(factID string) (bool, error)) (profile.Block, int, error)
}

// BulkVisibility optionally resolves the complete visibility overlay in one
// call so a context request does not re-read the overlay per candidate.
type BulkVisibility interface {
	Visibility
	SoftForgotten(context.Context, corememory.Scope) (map[string]struct{}, error)
}

var _ corememory.ContextProvider = (*Provider)(nil)

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
	if config.Recent.MaxItems == 0 {
		config.Recent.MaxItems = 8
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	provider := &Provider{
		Fusion: config.Fusion, Messages: config.Messages,
		Hydrator: config.Hydrator, Packer: config.Packer, Recent: config.Recent,
		ItemReranker:     config.ItemReranker,
		ScoreAdjuster:    config.ScoreAdjuster,
		Generations:      config.Generations,
		SourceQuotes:     config.SourceQuotes,
		ObservedAtAnchor: config.ObservedAtAnchor,
		ExpandParents:    config.ExpandParents,
		RecallEvents:     config.RecallEvents, Visibility: config.Visibility, Clock: config.Clock,
		Profiles: config.Profiles,
	}
	if provider.Fusion != nil {
		provider.Fusion.ObserveLane = func(name string, elapsed time.Duration) {
			provider.recordStage("lane:"+name, time.Now().Add(-elapsed))
		}
	}
	return provider, nil
}

func (provider *Provider) Context(ctx context.Context, request corememory.ContextRequest) (corememory.ContextResult, error) {
	if provider == nil || provider.Fusion == nil || provider.Hydrator == nil || provider.Packer == nil {
		return corememory.ContextResult{}, corememory.NewError(corememory.KindNotConfigured, "context", errors.New("retrieval provider is incomplete"))
	}
	if ctx == nil {
		return corememory.ContextResult{}, corememory.NewError(corememory.KindInvalidRequest, "context", errors.New("context is required"))
	}
	if err := request.Validate(); err != nil {
		return corememory.ContextResult{}, err
	}
	// Bound caller-supplied limits before they shape any read or packing.
	request.Budget = ClampBudget(request.Budget)
	metadata := request.Metadata.Clone()
	if metadata == nil {
		metadata = corememory.Metadata{}
	}
	if request.ConversationID != "" {
		metadata["conversation_id"] = request.ConversationID
	}
	if !isNilInterface(provider.Generations) && request.ConversationID != "" {
		generation, found, generationErr := provider.Generations.ActiveGeneration(
			ctx, request.Scope, request.ConversationID)
		if generationErr != nil {
			// Reading the wrong generation is a correctness leak, not a
			// degradation: without the label a lane falls back to whatever its
			// own manifest holds. Fail the request instead of serving it.
			return corememory.ContextResult{}, corememory.NewError(
				corememory.KindProviderFailure, "context",
				fmt.Errorf("retrieval: resolve derivation generation: %w", generationErr))
		}
		if found {
			metadata["generation_id"] = generation
		}
	}
	if len(request.DatasetIDs) > 0 {
		encoded, _ := json.Marshal(request.DatasetIDs)
		metadata["dataset_ids"] = string(encoded)
	}
	limit, _ := EffectivePackLimits(request.Budget)
	limit *= 3
	candidates, diagnostics, recentCount, err := provider.readCandidates(ctx, request, metadata, limit)
	if err != nil {
		provider.setDiagnostics(diagnostics)
		return corememory.ContextResult{}, err
	}
	// The lanes filter their own state with this selector; the check below is
	// the same rule applied to what fusion hands back, so the two cannot
	// disagree about which axis a request named.
	selector, err := projectionstore.ParseSelector(metadata)
	if err != nil {
		provider.setDiagnostics(diagnostics)
		return corememory.ContextResult{}, corememory.NewError(corememory.KindInvalidRequest, "context", err)
	}
	hydrated := make([]corememory.ContextItem, 0, len(candidates))
	contentTruncated := false
	eligible := 0
	var hidden map[string]struct{}
	if bulk, ok := provider.Visibility.(BulkVisibility); ok && !isNilInterface(provider.Visibility) {
		overlay, overlayErr := bulk.SoftForgotten(ctx, request.Scope)
		if overlayErr != nil {
			// Fall back to per-item checks; the overlay is re-read only when
			// the bulk read fails.
			diagnostics = append(diagnostics, Diagnostic{Stage: "visibility", Err: overlayErr})
		} else {
			hidden = overlay
		}
	}
	var scoreOverlay map[string]float64
	if !isNilInterface(provider.ScoreAdjuster) {
		switch adjuster := provider.ScoreAdjuster.(type) {
		case ScopedScoreAdjuster:
			// The adjustments this request can be affected by are the ones
			// its candidates' conversations carry, and reading them costs
			// the conversations, not the scope.
			overlay, overlayErr := adjuster.ScoreOverlayFor(ctx, request.Scope, candidateConversations(candidates))
			if overlayErr != nil {
				diagnostics = append(diagnostics, Diagnostic{Stage: "score_adjust", Err: overlayErr})
			} else {
				scoreOverlay = overlay
			}
		case BulkScoreAdjuster:
			overlay, overlayErr := adjuster.ScoreOverlay(ctx, request.Scope)
			if overlayErr != nil {
				diagnostics = append(diagnostics, Diagnostic{Stage: "score_adjust", Err: overlayErr})
			} else {
				scoreOverlay = overlay
			}
		}
	}
	hydrateStarted := time.Now()
	defer func() { provider.recordStage("hydrate", hydrateStarted) }()
	for index, candidate := range candidates {
		isRecent := index < recentCount
		if (!isRecent && candidate.Score < request.MinScore) || !matchesRequest(candidate, request, selector) {
			continue
		}
		eligible++
		if err := ctx.Err(); err != nil {
			provider.setDiagnostics(diagnostics)
			return corememory.ContextResult{}, corememory.NewError(corememory.KindOperationInterrupted, "context", err)
		}
		item, hydrateErr := provider.Hydrator.Hydrate(ctx, request.Scope, candidate)
		if hydrateErr != nil {
			diagnostics = append(diagnostics, Diagnostic{Stage: "hydrate", Lane: candidate.Lane, Err: hydrateErr})
			continue
		}
		// The recent lane is never filtered: its items are the conversation's
		// own last turns, and the visibility overlay covers what the views
		// derived from them, not the turns themselves.
		if !isRecent && !isNilInterface(provider.Visibility) {
			visible, visibilityErr := provider.visible(ctx, request.Scope, item.Identity(request.Scope), hidden)
			if visibilityErr != nil {
				// An item whose visibility cannot be decided is not a
				// hidden one: the request reports the read and keeps the
				// item out, and the candidate stays counted -- a request
				// that could decide nothing fails below rather than
				// answering like an ordinary memory with the overlay's
				// items gone quiet.
				diagnostics = append(diagnostics, Diagnostic{Stage: "visibility", Lane: candidate.Lane, Err: visibilityErr})
				continue
			}
			if !visible {
				eligible--
				continue
			}
		}
		if isRecent {
			item.SourceClass = corememory.ContextSourceRecent
			// The newest turn is never dropped: bound it to the lane and
			// total budget instead, so the packer can still admit it.
			bounded, cut := TruncateItemContent(item, provider.recentTokenLimit(request), request.Budget.MaxChars)
			if cut {
				item = bounded
				contentTruncated = true
			}
		} else if item.Kind == corememory.ContextSummary {
			item.SourceClass = corememory.ContextSourceSummary
		} else {
			item.SourceClass = corememory.ContextSourceLongTerm
		}
		if len(scoreOverlay) > 0 && !isRecent {
			if factor, ok := scoreOverlay[item.Identity(request.Scope)]; ok && factor > 0 && factor < 1 {
				item.Score *= factor
			}
		}
		hydrated = append(hydrated, item)
		if provider.ExpandParents && !isRecent {
			if progressive, ok := provider.Hydrator.(hydrate.Progressive); ok {
				current := item
				visited := map[string]struct{}{current.Identity(request.Scope): {}}
				for depth := 0; current.ParentID != "" && depth < maxParentDepth; depth++ {
					parent, found, parentErr := progressive.Parent(ctx, request.Scope, current)
					if parentErr != nil {
						diagnostics = append(diagnostics, Diagnostic{Stage: "hydrate_parent", Lane: candidate.Lane, Err: parentErr})
						break
					}
					if !found {
						break
					}
					identity := parent.Identity(request.Scope)
					if _, seen := visited[identity]; seen {
						diagnostics = append(diagnostics, Diagnostic{
							Stage: "hydrate_parent", Lane: candidate.Lane,
							Err: errors.New("retrieval: parent chain contains a cycle"),
						})
						break
					}
					// A parent the overlay hides is where the chain ends.
					// Walking past it would serve what the chain above a
					// forgotten record holds -- the section a forgotten
					// chunk belongs to, the document a forgotten section
					// belongs to -- which is the content the hide was asked
					// to keep out, one level up.
					if !isNilInterface(provider.Visibility) {
						visible, visibilityErr := provider.visible(ctx, request.Scope, identity, hidden)
						if visibilityErr != nil {
							// Same failure, same direction: a parent whose
							// visibility cannot be decided is not served,
							// and the chain above it is not walked either.
							diagnostics = append(diagnostics, Diagnostic{Stage: "visibility", Lane: candidate.Lane, Err: visibilityErr})
							break
						}
						if !visible {
							break
						}
					}
					visited[identity] = struct{}{}
					parent.SourceClass = item.SourceClass
					hydrated = append(hydrated, parent)
					current = parent
				}
			}
		}
	}
	if eligible > 0 && len(hydrated) == 0 {
		provider.setDiagnostics(diagnostics)
		return corememory.ContextResult{}, corememory.NewError(
			corememory.KindProviderFailure, "context",
			errors.New("retrieval: every eligible candidate failed hydration or could not be decided"),
		)
	}
	if provider.ItemReranker != nil && len(hydrated) > 0 && strings.TrimSpace(request.Query) != "" {
		reranked, rerankErr := provider.ItemReranker.RerankItems(ctx, request.Query, cloneContextItems(hydrated))
		if rerankErr != nil {
			diagnostics = append(diagnostics, Diagnostic{Stage: "rerank_items", Err: rerankErr})
			if ctx.Err() != nil {
				provider.setDiagnostics(diagnostics)
				return corememory.ContextResult{}, corememory.NewError(corememory.KindOperationInterrupted, "context", ctx.Err())
			}
		} else if normalized, validationErr := normalizeRerankedItems(hydrated, reranked); validationErr != nil {
			diagnostics = append(diagnostics, Diagnostic{Stage: "rerank_items", Err: validationErr})
		} else {
			hydrated = normalized
		}
	}
	if provider.SourceQuotes > 0 && provider.Messages != nil {
		quotesStarted := time.Now()
		hydrated = provider.withSourceQuotes(ctx, request.Scope, hydrated)
		provider.recordStage("quotes", quotesStarted)
	}
	// The resident block is added last, after the reranker and the source
	// quotes: it is not a candidate, so nothing that decides about candidates
	// may decide about it, and a reranker that only knows the lanes cannot
	// drop what was never theirs to rank.
	if item, ok := provider.profileItem(ctx, request, hidden, &diagnostics); ok {
		hydrated = append(hydrated, item)
	}
	if provider.ObservePool != nil {
		provider.ObservePool(request, hydrated)
	}
	packStarted := time.Now()
	result, err := provider.Packer.Pack(ctx, hydrated, request.Budget)
	provider.recordStage("pack", packStarted)
	if err != nil {
		diagnostics = append(diagnostics, Diagnostic{Stage: "pack", Err: err})
		provider.setDiagnostics(diagnostics)
		if ctx.Err() != nil {
			return corememory.ContextResult{}, corememory.NewError(corememory.KindOperationInterrupted, "context", ctx.Err())
		}
		return corememory.ContextResult{}, corememory.NewError(corememory.KindInternal, "context", fmt.Errorf("retrieval: pack: %w", err))
	}
	if err := result.Validate(); err != nil {
		diagnostics = append(diagnostics, Diagnostic{Stage: "pack", Err: err})
		provider.setDiagnostics(diagnostics)
		return corememory.ContextResult{}, corememory.NewError(corememory.KindInternal, "context", fmt.Errorf("retrieval: pack: %w", err))
	}
	result.Truncated = result.Truncated || contentTruncated
	result.RecallEventID = request.RecallEventID
	if request.RecallEventID != "" && !isNilInterface(provider.RecallEvents) {
		scores := make(map[string]float64)
		for _, item := range result.Items {
			if item.SourceClass != corememory.ContextSourceLongTerm && item.SourceClass != corememory.ContextSourceSummary {
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
			event := corememory.RecallEvent{ID: request.RecallEventID, Scope: request.Scope, ItemIDs: ids, Scores: values, Time: provider.Clock().UTC()}
			if recordErr := provider.RecallEvents.RecordRecall(ctx, event); recordErr != nil {
				diagnostics = append(diagnostics, Diagnostic{Stage: "reinforce", Err: recordErr})
			}
		}
	}
	provider.setDiagnostics(diagnostics)
	return result, nil
}

func (provider *Provider) readCandidates(ctx context.Context, request corememory.ContextRequest, metadata corememory.Metadata, limit int) ([]component.Candidate, []Diagnostic, int, error) {
	recent, err := provider.recentCandidates(ctx, request)
	if err != nil {
		return nil, nil, 0, corememory.NewError(corememory.KindProviderFailure, "context", fmt.Errorf("retrieval: recent: %w", err))
	}
	candidates := append([]component.Candidate(nil), recent...)
	diagnostics := make([]Diagnostic, 0)
	if strings.TrimSpace(request.Query) != "" {
		// One query per request: query decomposition was measured on LoCoMo as a
		// net negative (lenient -1.4pp, retrieval latency +65%) and was removed.
		queries := []string{request.Query}
		searchStarted := time.Now()
		hybrid, primaryErr := provider.searchQueries(ctx, request, metadata, limit, queries, &diagnostics)
		provider.recordStage("search", searchStarted)
		if stateErr := laneStateFailure(diagnostics); stateErr != nil {
			// A lane that cannot read the state it is bound to read did not
			// "find nothing": its recall is missing from this answer, and
			// nothing in the result would say so. Serving the remaining lanes
			// would answer like an ordinary memory, so the request fails
			// instead -- the same choice the derivation generation makes below
			// -- and Verify names the condition while Repair is the way out.
			return nil, diagnostics, 0, corememory.NewError(corememory.KindProviderFailure, "context", stateErr)
		}
		if primaryErr != nil {
			if ctx.Err() != nil {
				return nil, diagnostics, len(recent), corememory.NewError(
					corememory.KindOperationInterrupted, "context", ctx.Err(),
				)
			}
			if len(recent) == 0 && len(hybrid) == 0 {
				return nil, diagnostics, 0, primaryErr
			}
			diagnostics = append(diagnostics, Diagnostic{Stage: "search", Err: primaryErr})
		}
		candidates = append(candidates, hybrid...)
	}
	return candidates, diagnostics, len(recent), nil
}

// laneStateFailure returns the first lane that held state it could not read,
// if any. A lane with no state at all is not one: it reports NotFound, which
// is what a scope that was never indexed answers with, and those lanes simply
// contribute nothing. What is refused is the answer that silently lost a lane
// whose state exists.
func laneStateFailure(diagnostics []Diagnostic) error {
	for _, diagnostic := range diagnostics {
		var state *component.StateError
		if errors.As(diagnostic.Err, &state) {
			return fmt.Errorf("retrieval: lane %q cannot read its state: %w", diagnostic.Lane, diagnostic.Err)
		}
	}
	return nil
}

// searchQueries runs the request's queries and merges their candidates. It
// takes one query today; the merge stays because the caller is the single place
// that would grow sub-queries again. With one query the fused score is kept as-is; with
// several queries the merge is reciprocal-rank fusion, so evidence that ranks
// highly for a sub-query can outrank primary-query noise instead of being
// flattened by a max-score merge. The primary query's error is returned only
// when no other query produced candidates, so a flaky sub-query never fails
// the whole request.
func (provider *Provider) searchQueries(
	ctx context.Context,
	request corememory.ContextRequest,
	metadata corememory.Metadata,
	limit int,
	queries []string,
	diagnostics *[]Diagnostic,
) ([]component.Candidate, error) {
	const rrfK = 60.0
	type aggregate struct {
		candidate component.Candidate
		score     float64
	}
	merged := make(map[string]*aggregate, limit)
	successful := 0
	var primaryErr error
	for index, query := range queries {
		fused, searchErr := provider.Fusion.SearchDetailed(ctx, component.SearchRequest{
			Scope: request.Scope, Query: query, Limit: limit, Metadata: metadata,
		})
		for _, diagnostic := range fused.Diagnostics {
			*diagnostics = append(*diagnostics, Diagnostic{Stage: "search", Lane: diagnostic.Lane, Err: diagnostic.Err})
		}
		if searchErr != nil {
			if index == 0 {
				primaryErr = searchErr
			} else {
				*diagnostics = append(*diagnostics, Diagnostic{
					Stage: "search", Err: fmt.Errorf("query %q: %w", query, searchErr),
				})
			}
			continue
		}
		successful++
		for rank, candidate := range fused.Candidates {
			key := candidateIdentity(candidate)
			item := merged[key]
			if item == nil {
				clone := candidate
				item = &aggregate{candidate: clone}
				merged[key] = item
			}
			// Candidates arrive sorted by score, so the slice index is the
			// query-local rank used by the cross-query RRF merge.
			item.score += 1 / (rrfK + float64(rank+1))
		}
	}
	results := make([]component.Candidate, 0, len(merged))
	if successful == 1 && len(queries) == 1 {
		// Single-query runs keep the fused score so MinScore semantics stay
		// unchanged for callers that never enable decomposition.
		for _, item := range merged {
			results = append(results, item.candidate)
		}
	} else if successful > 0 {
		maxPossible := float64(successful) / (rrfK + 1)
		for _, item := range merged {
			item.candidate.Score = item.score / maxPossible
			results = append(results, item.candidate)
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return candidateIdentity(results[i]) < candidateIdentity(results[j])
		}
		return results[i].Score > results[j].Score
	})
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, primaryErr
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

// candidateConversations returns the conversations a candidate set can draw
// adjustments from: the ones its addresses name, deduplicated. A candidate
// whose address names none is a lane outside the overlay's scope and is
// skipped; the overlay read that needs this list reads one bucket per
// conversation instead of the scope's whole document.
func candidateConversations(candidates []component.Candidate) []string {
	conversations := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		conversation := strings.TrimSpace(candidate.Address.ConversationID)
		if conversation == "" {
			continue
		}
		if _, exists := seen[conversation]; exists {
			continue
		}
		seen[conversation] = struct{}{}
		conversations = append(conversations, conversation)
	}
	return conversations
}

// normalizeRerankedItems validates an item-reranker result: every returned
// item must be one of the input items (by identity) exactly once. Items the
// reranker omitted are appended in their original order, so a partially
// ordered result still keeps its full candidate set.
func normalizeRerankedItems(input, output []corememory.ContextItem) ([]corememory.ContextItem, error) {
	allowed := make(map[string]corememory.ContextItem, len(input))
	for _, item := range input {
		key := itemIdentityKey(item)
		allowed[key] = item
	}
	result := make([]corememory.ContextItem, 0, len(input))
	seen := make(map[string]struct{}, len(output))
	for _, item := range output {
		key := itemIdentityKey(item)
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("retrieval: item reranker returned an unknown item %q", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("retrieval: item reranker returned duplicate item %q", key)
		}
		seen[key] = struct{}{}
		result = append(result, item)
	}
	for _, item := range input {
		if _, ok := seen[itemIdentityKey(item)]; ok {
			continue
		}
		result = append(result, item)
	}
	return result, nil
}

func itemIdentityKey(item corememory.ContextItem) string {
	if !item.Address.IsZero() {
		return item.Address.Key()
	}
	return string(item.Kind) + "\x00" + item.ID
}

func cloneContextItems(items []corememory.ContextItem) []corememory.ContextItem {
	cloned := make([]corememory.ContextItem, len(items))
	for index, item := range items {
		cloned[index] = item.Clone()
	}
	return cloned
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

func (provider *Provider) recentCandidates(ctx context.Context, request corememory.ContextRequest) ([]component.Candidate, error) {
	if provider.Messages == nil || request.ConversationID == "" {
		return []component.Candidate{}, nil
	}
	maxItems := provider.Recent.MaxItems
	if request.RecentLimit > 0 {
		maxItems = request.RecentLimit
	}
	if maxItems > MaxRecentItems {
		maxItems = MaxRecentItems
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
	if maxTokens > MaxRecentTokens {
		maxTokens = MaxRecentTokens
	}
	if maxTokens > 0 {
		used := 0
		start := len(records)
		for start > 0 {
			count := pack.ContentTokens(records[start-1].Message.Content)
			if used+count > maxTokens {
				break
			}
			used += count
			start--
		}
		if start == len(records) && start > 0 {
			// A single oversized newest record is kept here and bounded
			// after hydration (see Context): dropping the lane entirely
			// would silently lose the most recent turn.
			start--
		}
		records = records[start:]
	}
	result := make([]component.Candidate, len(records))
	for index, record := range records {
		result[index] = component.Candidate{
			ID: record.ID, Lane: "recent", Name: "message", Score: 1,
			Source: corememory.SourceRef{
				Kind: corememory.SourceMessage, ID: record.ConversationID + "/" + record.ID,
				Revision: strconv.FormatUint(record.Seq, 10),
			},
			Address: component.CandidateAddress{
				Kind: corememory.ContextRawMessage, ConversationID: record.ConversationID, ItemID: record.ID,
			},
		}
	}
	return result, nil
}

// visible reports whether one identity may be served, against the overlay the
// request already read. The bulk overlay is the read a request pays for; a
// substrate without one, or one whose bulk read failed, falls back to the
// per-identity read, which is why both are asked the same question here.
//
// The three answers are three, not two. A hidden item is a decision, and the
// caller drops it. An unreadable overlay is not a decision and not "nothing
// hidden": the error is reported, the item is not served, and the caller keeps
// it counted, so a request that decided nothing fails instead of answering as
// if the overlay were empty. That is the direction a visibility overlay has to
// fail in -- an entry nobody can read was a statement that the item was meant
// to be unreachable -- and it is why this returns an error rather than folding
// it into "not visible".
func (provider *Provider) visible(
	ctx context.Context,
	scope corememory.Scope,
	identity string,
	hidden map[string]struct{},
) (bool, error) {
	if hidden != nil {
		_, forgotten := hidden[identity]
		return !forgotten, nil
	}
	return provider.Visibility.Visible(ctx, scope, identity)
}

// profileItem returns the conversation's resident block as one context item, or
// false when this request serves none. The block is an addition to a request
// rather than a candidate: a deployment without one serves exactly what it
// served before, a conversation without one is served the same way, and a block
// that cannot be read is reported and left out -- the derived block must not be
// what fails a request the canonical views can answer.
//
// The lines are a pass's reading of the corpus, so a line is served only while
// the facts it was judged from are: the overlay that drops a forgotten fact
// drops the line with it, and a provenance the read path cannot decide about
// is not served, which is the direction the visibility overlay fails in.
func (provider *Provider) profileItem(
	ctx context.Context,
	request corememory.ContextRequest,
	hidden map[string]struct{},
	diagnostics *[]Diagnostic,
) (corememory.ContextItem, bool) {
	if isNilInterface(provider.Profiles) || request.ConversationID == "" {
		return corememory.ContextItem{}, false
	}
	var hide func(string) (bool, error)
	if !isNilInterface(provider.Visibility) {
		hide = func(factID string) (bool, error) {
			if strings.TrimSpace(factID) == "" {
				return false, nil
			}
			identity := corememory.ContextAddress{
				Kind: corememory.ContextFact, ConversationID: request.ConversationID, ItemID: factID,
			}.Identity(request.Scope)
			visible, err := provider.visible(ctx, request.Scope, identity, hidden)
			if err != nil {
				return true, err
			}
			return !visible, nil
		}
	}
	started := time.Now()
	block, _, err := provider.Profiles.BlockServing(ctx, request.Scope, request.ConversationID, hide)
	provider.recordStage("profile", started)
	if err != nil {
		*diagnostics = append(*diagnostics, Diagnostic{Stage: "profile", Lane: "profile", Err: err})
		return corememory.ContextItem{}, false
	}
	if strings.TrimSpace(block.Text) == "" || len(block.Sources) == 0 {
		return corememory.ContextItem{}, false
	}
	return corememory.ContextItem{
		ID: request.ConversationID,
		Address: corememory.ContextAddress{
			Kind: corememory.ContextProfile, ConversationID: request.ConversationID, ItemID: request.ConversationID,
		},
		Kind:        corememory.ContextProfile,
		Content:     coremessage.NewTextContent(block.Text),
		Score:       1,
		Sources:     append([]corememory.SourceRef(nil), block.Sources...),
		SourceClass: corememory.ContextSourceProfile,
	}, true
}

// recentTokenLimit bounds one recent item to both the recent lane budget and
// the total pack budget, so a bounded newest turn always survives packing.
func (provider *Provider) recentTokenLimit(request corememory.ContextRequest) int {
	limit := provider.Recent.MaxTokens
	if request.RecentMaxTokens > 0 {
		limit = request.RecentMaxTokens
	}
	if limit > MaxRecentTokens {
		limit = MaxRecentTokens
	}
	_, packTokens := EffectivePackLimits(request.Budget)
	if limit <= 0 || packTokens < limit {
		limit = packTokens
	}
	return limit
}

// withSourceQuotes folds the canonical message text a fact was derived from
// (at most SourceQuotes per fact) into the fact's own content, so consumers see
// the wording the conversation used and not only the paraphrase. The quote
// rides along with the fact rather than becoming its own item: the pack budget
// is an item cap first (measured: every question packs exactly MaxItems), and a
// separate quote item is the first thing that cap drops.
func (provider *Provider) withSourceQuotes(ctx context.Context, scope corememory.Scope, items []corememory.ContextItem) []corememory.ContextItem {
	enriched := make([]corememory.ContextItem, 0, len(items))
	for _, item := range items {
		if item.Kind != corememory.ContextFact {
			enriched = append(enriched, item)
			continue
		}
		var quotes []string
		added := 0
		for _, source := range item.Sources {
			if added >= provider.SourceQuotes {
				break
			}
			if source.Kind != corememory.SourceMessage {
				continue
			}
			conversationID, messageID, ok := splitMessageSource(source.ID)
			if !ok {
				continue
			}
			text, ok := provider.quoteText(ctx, scope, source.ID, conversationID, messageID)
			if !ok {
				continue
			}
			added++
			quotes = append(quotes, text)
		}
		if len(quotes) == 0 {
			enriched = append(enriched, item)
			continue
		}
		item.Content = coremessage.NewTextContent(item.Content.Text() + "\nSource turn: " + strings.Join(quotes, " | "))
		enriched = append(enriched, item)
	}
	return enriched
}

// quoteText returns one canonical message's text, reusing a process-wide cache:
// messages are immutable, and the source-quote fold reads the same messages on
// every request.
func (provider *Provider) quoteText(
	ctx context.Context,
	scope corememory.Scope,
	sourceID, conversationID, messageID string,
) (string, bool) {
	key := scope.HardPartitionKey() + "\x00" + sourceID
	provider.quoteMu.Lock()
	if text, ok := provider.quoteTexts[key]; ok {
		provider.quoteMu.Unlock()
		return text, text != ""
	}
	provider.quoteMu.Unlock()
	record, found, err := provider.Messages.Get(ctx, scope, conversationID, messageID)
	if err != nil || !found {
		return "", false
	}
	text := strings.TrimSpace(record.Message.Content.Text())
	if provider.ObservedAtAnchor {
		text = record.AnchoredText(text)
	}
	provider.quoteMu.Lock()
	switch {
	case provider.quoteTexts == nil:
		provider.quoteTexts = make(map[string]string)
	case len(provider.quoteTexts) >= maxQuoteTexts:
		provider.quoteTexts = make(map[string]string)
	}
	provider.quoteTexts[key] = text
	provider.quoteMu.Unlock()
	return text, text != ""
}

// splitMessageSource splits a "conversation/message" provenance id.
func splitMessageSource(id string) (string, string, bool) {
	index := strings.LastIndex(id, "/")
	if index <= 0 || index == len(id)-1 {
		return "", "", false
	}
	return id[:index], id[index+1:], true
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

// matchesRequest re-checks one fused candidate against the request's compiled
// selector. A candidate whose address lives in a locator envelope rather than
// in these fields cannot be judged by kind, so all that is left of it is the
// conversation it reports -- and the lane that produced it already applied the
// selector to its own state.
func matchesRequest(
	candidate component.Candidate,
	request corememory.ContextRequest,
	selector projectionstore.Selector,
) bool {
	address := candidate.Address
	if address.Kind == "" {
		return request.ConversationID == "" || address.ConversationID == "" ||
			address.ConversationID == request.ConversationID
	}
	return selector.Matches(address)
}
