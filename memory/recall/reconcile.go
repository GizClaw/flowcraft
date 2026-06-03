package recall

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Reconciler is the explicit operator entry point for multi-scope repair.
//
// It composes existing single-scope primitives and does not own background
// goroutines. Single-scope methods such as RebuildAll and ExpireRetired keep
// their current semantics; callers opt into multi-scope work here. Reconciler
// is not a scheduler and never mutates async queues; it only rebuilds or audits
// derived work from canonical facts when invoked.
type Reconciler interface {
	ReconcileAsyncSemantic(ctx context.Context, scope Scope, opts AsyncSemanticReconcileOptions) (AsyncSemanticReconcileResult, error)
	ReconcileSideEffects(ctx context.Context, scope Scope, opts SideEffectReconcileOptions) (SideEffectReconcileResult, error)
	ReconcileScopes(ctx context.Context, scopes []Scope, opts ReconcileOptions) (ReconcileResult, error)
	ReconcileRuntime(ctx context.Context, runtimeID string, opts ReconcileOptions) (ReconcileResult, error)
}

// AsyncSemanticReconcileStatus classifies the canonical derivation state for
// one async semantic request.
type AsyncSemanticReconcileStatus string

const (
	AsyncSemanticReconcileCompleted     AsyncSemanticReconcileStatus = "completed"
	AsyncSemanticReconcilePending       AsyncSemanticReconcileStatus = "pending"
	AsyncSemanticReconcileSkipped       AsyncSemanticReconcileStatus = "skipped"
	AsyncSemanticReconcileUnrecoverable AsyncSemanticReconcileStatus = "unrecoverable"
)

// AsyncSemanticReconcileOptions controls canonical async semantic recovery.
type AsyncSemanticReconcileOptions struct {
	// Now anchors canonical-active and retired checks. Zero defaults to time.Now.
	Now time.Time
	// RequeueMissing re-enqueues a minimal async semantic job when canonical
	// episode facts exist but no semantic derivation facts are present. The
	// rebuilt job intentionally omits enqueue-time RecentMessages/anchors that
	// cannot be recovered from canonical facts.
	RequeueMissing bool
}

// AsyncSemanticReconcileResult summarizes one async semantic recovery audit.
type AsyncSemanticReconcileResult struct {
	Scope         Scope
	Episodes      int
	Requests      int
	Completed     int
	Pending       int
	Skipped       int
	Unrecoverable int
	Results       []AsyncSemanticRequestReconcileResult
}

// AsyncSemanticRequestReconcileResult is the per-request row in
// AsyncSemanticReconcileResult.
type AsyncSemanticRequestReconcileResult struct {
	RequestID       string
	EpisodeFactIDs  []string
	SemanticFactIDs []string
	Status          AsyncSemanticReconcileStatus
	Reason          string
}

// SideEffectReconcileOptions controls one canonical-driven side-effect repair.
type SideEffectReconcileOptions struct {
	// ProjectionName limits repair to one projection. Empty rebuilds every
	// registered projection.
	ProjectionName string
}

// SideEffectReconcileResult summarizes one canonical-driven repair pass.
type SideEffectReconcileResult struct {
	Scope          Scope
	FactsScanned   int
	ProjectionName string
	Rebuilt        bool
}

// ReconcileOptions controls one explicit reconcile pass.
type ReconcileOptions struct {
	// ExpireRetired runs ExpireRetired before rebuilding projections.
	ExpireRetired bool
	// Now anchors TTL sweeps. Zero defaults to time.Now when ExpireRetired is true.
	Now time.Time
	// Enumerator overrides the memory store's optional ScopeEnumerator in
	// ReconcileRuntime. ReconcileScopes ignores this field.
	Enumerator ScopeEnumerator
	// StopOnError stops after the first scope failure. By default reconcile
	// records the failure and continues with the remaining scopes.
	StopOnError bool
}

// ReconcileResult summarizes a multi-scope reconcile pass.
type ReconcileResult struct {
	Scopes  int
	Rebuilt int
	Expired int
	Failed  int
	Results []ScopeReconcileResult
}

// ScopeReconcileResult is the per-scope row in ReconcileResult.
type ScopeReconcileResult struct {
	Scope        Scope
	FactsScanned int
	Rebuilt      bool
	Expired      int
	Err          string
}

// ScopeReconcileFailure keeps the typed error for one failed scope so callers
// can still use errors.Is / errors.As on ReconcileError.
type ScopeReconcileFailure struct {
	Scope Scope
	Err   error
}

// ReconcileError reports per-scope failures while preserving their underlying
// errors. The partial ReconcileResult is returned alongside it.
type ReconcileError struct {
	Failures []ScopeReconcileFailure
}

func (e ReconcileError) Error() string {
	if len(e.Failures) == 0 {
		return "recall reconcile failed"
	}
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		parts = append(parts, fmt.Sprintf("%s: %s", f.Scope.PartitionKey(), f.Err))
	}
	return "recall reconcile failed: " + strings.Join(parts, "; ")
}

// Unwrap exposes the per-scope causes for errors.Is / errors.As.
func (e ReconcileError) Unwrap() []error {
	out := make([]error, 0, len(e.Failures))
	for _, f := range e.Failures {
		if f.Err != nil {
			out = append(out, f.Err)
		}
	}
	return out
}

// NewReconciler returns the multi-scope operator API when mem is this package's
// Memory implementation.
func NewReconciler(mem Memory) (Reconciler, bool) {
	m, ok := mem.(*memory)
	if !ok || m == nil {
		return nil, false
	}
	return m, true
}

func (m *memory) ReconcileRuntime(ctx context.Context, runtimeID string, opts ReconcileOptions) (ReconcileResult, error) {
	if runtimeID == "" {
		return ReconcileResult{}, errdefs.Validationf("recall.ReconcileRuntime: runtime_id is required")
	}
	enumerator := opts.Enumerator
	if enumerator == nil {
		var ok bool
		enumerator, ok = m.store.(ScopeEnumerator)
		if !ok {
			return ReconcileResult{}, errdefs.Validationf(
				"recall.ReconcileRuntime: requires ScopeEnumerator")
		}
	}
	scopes, err := enumerator.ListScopes(ctx, ScopeListQuery{RuntimeID: runtimeID})
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("recall.ReconcileRuntime: list scopes: %w", err)
	}
	return m.ReconcileScopes(ctx, scopes, opts)
}

// ReconcileAsyncSemantic audits async semantic derivation state from canonical
// facts. It does not modify the queue or synthesize replacement jobs: enqueue
// payloads can include point-in-time context that is not always recoverable
// from episode facts alone.
func (m *memory) ReconcileAsyncSemantic(ctx context.Context, scope Scope, opts AsyncSemanticReconcileOptions) (AsyncSemanticReconcileResult, error) {
	if err := ctx.Err(); err != nil {
		return AsyncSemanticReconcileResult{}, err
	}
	if scope.RuntimeID == "" {
		return AsyncSemanticReconcileResult{}, errdefs.Validationf("recall.ReconcileAsyncSemantic: scope.runtime_id is required")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	episodes, err := m.store.List(ctx, scope, ListQuery{
		Kinds:             []FactKind{FactEpisode},
		IncludeSuperseded: true,
	})
	if err != nil {
		return AsyncSemanticReconcileResult{}, fmt.Errorf("recall.ReconcileAsyncSemantic: list episodes: %w", err)
	}
	result := AsyncSemanticReconcileResult{
		Scope:    Scope{RuntimeID: scope.RuntimeID, UserID: scope.UserID},
		Episodes: len(episodes),
	}
	grouped := make(map[string][]TemporalFact)
	for _, episode := range episodes {
		requestID := episode.Origin.RequestID
		if requestID == "" || episode.Origin.Kind != OriginKindEpisode {
			row := AsyncSemanticRequestReconcileResult{
				EpisodeFactIDs: []string{episode.ID},
				Status:         AsyncSemanticReconcileSkipped,
				Reason:         "episode fact missing async origin request id",
			}
			result.Skipped++
			result.Results = append(result.Results, row)
			continue
		}
		grouped[requestID] = append(grouped[requestID], episode)
	}
	requestIDs := make([]string, 0, len(grouped))
	for requestID := range grouped {
		requestIDs = append(requestIDs, requestID)
	}
	sort.Strings(requestIDs)
	for _, requestID := range requestIDs {
		row, err := m.reconcileAsyncSemanticRequest(ctx, scope, requestID, grouped[requestID], now, opts)
		if err != nil {
			return result, err
		}
		switch row.Status {
		case AsyncSemanticReconcileCompleted:
			result.Completed++
		case AsyncSemanticReconcilePending:
			result.Pending++
		case AsyncSemanticReconcileSkipped:
			result.Skipped++
		case AsyncSemanticReconcileUnrecoverable:
			result.Unrecoverable++
		}
		result.Results = append(result.Results, row)
	}
	result.Requests = len(result.Results)
	return result, nil
}

func (m *memory) reconcileAsyncSemanticRequest(ctx context.Context, scope Scope, requestID string, episodes []TemporalFact, now time.Time, opts AsyncSemanticReconcileOptions) (AsyncSemanticRequestReconcileResult, error) {
	row := AsyncSemanticRequestReconcileResult{
		RequestID:      requestID,
		EpisodeFactIDs: reconcileFactIDs(episodes),
	}
	if len(episodes) == 0 {
		row.Status = AsyncSemanticReconcileSkipped
		row.Reason = "request has no episode facts"
		return row, nil
	}
	for _, episode := range episodes {
		if !domain.IsCanonicalActive(episode, now) || domain.IsRetired(episode, now) {
			row.Status = AsyncSemanticReconcileUnrecoverable
			row.Reason = "episode fact is not eligible for semantic derivation"
			return row, nil
		}
	}
	byOrigin, err := m.store.FindByOriginRequestID(ctx, scope, requestID)
	if err != nil {
		return row, fmt.Errorf("recall.ReconcileAsyncSemantic: find origin %q: %w", requestID, err)
	}
	for _, fact := range byOrigin {
		if fact.Origin.Kind == OriginKindSemanticDerivation {
			row.SemanticFactIDs = append(row.SemanticFactIDs, fact.ID)
		}
	}
	if len(row.SemanticFactIDs) > 0 {
		sort.Strings(row.SemanticFactIDs)
		row.Status = AsyncSemanticReconcileCompleted
		return row, nil
	}
	row.Status = AsyncSemanticReconcilePending
	row.Reason = "semantic derivation facts not found"
	if opts.RequeueMissing {
		if m.asyncSemanticQueue == nil {
			row.Reason = "semantic derivation facts not found; async semantic queue not configured"
			return row, nil
		}
		requeued, err := m.requeueAsyncSemanticRequest(ctx, scope, requestID, episodes)
		if err != nil {
			return row, err
		}
		if !requeued {
			row.Status = AsyncSemanticReconcileSkipped
			row.Reason = "semantic derivation facts not found; async semantic queue cannot requeue terminal jobs"
			return row, nil
		}
		row.Reason = "semantic derivation facts not found; requeued minimal async job"
	}
	return row, nil
}

func (m *memory) requeueAsyncSemanticRequest(ctx context.Context, scope Scope, requestID string, episodes []TemporalFact) (bool, error) {
	if m == nil || m.asyncSemanticQueue == nil {
		return false, nil
	}
	ids := reconcileFactIDs(episodes)
	if requestID == "" || len(ids) == 0 {
		return false, nil
	}
	observedAt := time.Time{}
	for _, episode := range episodes {
		if observedAt.IsZero() || (!episode.ObservedAt.IsZero() && episode.ObservedAt.Before(observedAt)) {
			observedAt = episode.ObservedAt
		}
	}
	requeue, ok := m.asyncSemanticQueue.(port.AsyncSemanticRequeueQueue)
	if !ok {
		return false, nil
	}
	_, requeued, err := requeue.Requeue(ctx, port.AsyncSemanticJob{
		RequestID:      requestID,
		Scope:          Scope{RuntimeID: scope.RuntimeID, UserID: scope.UserID},
		EpisodeFactIDs: ids,
		ObservedAt:     observedAt,
	})
	return requeued, err
}

// ReconcileSideEffects repairs derived side effects from canonical facts.
//
// The first implementation repairs projections directly by replaying the
// canonical TemporalStore snapshot. It deliberately does not synthesize
// SideEffectOutbox jobs: queue payloads can contain enqueue-time context, so
// missing jobs need a narrower recovery contract than projection rebuild.
func (m *memory) ReconcileSideEffects(ctx context.Context, scope Scope, opts SideEffectReconcileOptions) (SideEffectReconcileResult, error) {
	if err := ctx.Err(); err != nil {
		return SideEffectReconcileResult{}, err
	}
	if scope.RuntimeID == "" {
		return SideEffectReconcileResult{}, errdefs.Validationf("recall.ReconcileSideEffects: scope.runtime_id is required")
	}
	facts, err := m.store.List(ctx, scope, ListQuery{IncludeSuperseded: true})
	if err != nil {
		return SideEffectReconcileResult{}, fmt.Errorf("recall.ReconcileSideEffects: list canonical facts: %w", err)
	}
	result := SideEffectReconcileResult{
		Scope:          Scope{RuntimeID: scope.RuntimeID, UserID: scope.UserID},
		FactsScanned:   len(facts),
		ProjectionName: opts.ProjectionName,
	}
	if opts.ProjectionName != "" {
		if err := m.RebuildProjection(ctx, scope, opts.ProjectionName); err != nil {
			return result, err
		}
		result.Rebuilt = true
		return result, nil
	}
	if err := m.RebuildAll(ctx, scope); err != nil {
		return result, err
	}
	result.Rebuilt = true
	return result, nil
}

func (m *memory) ReconcileScopes(ctx context.Context, scopes []Scope, opts ReconcileOptions) (ReconcileResult, error) {
	if err := ctx.Err(); err != nil {
		return ReconcileResult{}, err
	}
	scopes, err := normalizeReconcileScopes(scopes)
	if err != nil {
		return ReconcileResult{}, err
	}
	now := opts.Now
	if opts.ExpireRetired && now.IsZero() {
		now = time.Now()
	}
	result := ReconcileResult{Scopes: len(scopes), Results: make([]ScopeReconcileResult, 0, len(scopes))}
	var failures []ScopeReconcileFailure
	for _, scope := range scopes {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		row := ScopeReconcileResult{Scope: scope}
		if opts.ExpireRetired {
			n, err := m.ExpireRetired(ctx, scope, now)
			row.Expired = n
			result.Expired += n
			if err != nil {
				row.Err = err.Error()
				result.Failed++
				failures = append(failures, ScopeReconcileFailure{Scope: scope, Err: err})
				result.Results = append(result.Results, row)
				if opts.StopOnError {
					return result, ReconcileError{Failures: failures}
				}
				continue
			}
		}
		reconciled, err := m.ReconcileSideEffects(ctx, scope, SideEffectReconcileOptions{})
		row.FactsScanned = reconciled.FactsScanned
		if err != nil {
			row.Err = err.Error()
			result.Failed++
			failures = append(failures, ScopeReconcileFailure{Scope: scope, Err: err})
			result.Results = append(result.Results, row)
			if opts.StopOnError {
				return result, ReconcileError{Failures: failures}
			}
			continue
		}
		row.Rebuilt = reconciled.Rebuilt
		result.Rebuilt++
		result.Results = append(result.Results, row)
	}
	if len(failures) > 0 {
		return result, ReconcileError{Failures: failures}
	}
	return result, nil
}

func normalizeReconcileScopes(scopes []Scope) ([]Scope, error) {
	out := make([]Scope, 0, len(scopes))
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		normalized := Scope{RuntimeID: scope.RuntimeID, UserID: scope.UserID}
		if normalized.RuntimeID == "" {
			return nil, errdefs.Validationf("recall.ReconcileScopes: scope.runtime_id is required")
		}
		key := normalized.PartitionKey()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, normalized)
	}
	return out, nil
}

func reconcileFactIDs(facts []TemporalFact) []string {
	out := make([]string, 0, len(facts))
	for _, fact := range facts {
		if fact.ID != "" {
			out = append(out, fact.ID)
		}
	}
	sort.Strings(out)
	return out
}

var _ Reconciler = (*memory)(nil)
