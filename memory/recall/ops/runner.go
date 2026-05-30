package ops

import (
	"context"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Runner owns operator loops for one configured recall.Memory.
type Runner struct {
	side      recall.SideEffectProcessor
	async     recall.AsyncSemanticProcessor
	reconcile recall.Reconciler
	ready     recall.ReadinessObserver
	cfg       Config
}

// NewRunner creates an embeddable ops runner. It never starts goroutines; call
// Run explicitly when the surrounding process is ready to own the lifecycle.
func NewRunner(mem recall.Memory, opts ...Option) (*Runner, error) {
	if mem == nil {
		return nil, validationf("nil memory")
	}
	cfg := DefaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.IdleInterval <= 0 {
		cfg.IdleInterval = defaultIdleInterval
	}
	if cfg.ErrorBackoff <= 0 {
		cfg.ErrorBackoff = defaultErrorBackoff
	}
	if cfg.MaxConcurrentScopes <= 0 {
		cfg.MaxConcurrentScopes = defaultConcurrency
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	side, _ := recall.NewSideEffectProcessor(mem)
	async, _ := recall.NewAsyncSemanticProcessor(mem)
	reconciler, _ := recall.NewReconciler(mem)
	ready, _ := mem.(recall.ReadinessObserver)
	return &Runner{
		side:      side,
		async:     async,
		reconcile: reconciler,
		ready:     ready,
		cfg:       cfg,
	}, nil
}

// Run continuously drains the configured target until ctx is cancelled.
func (r *Runner) Run(ctx context.Context, opts RunOptions) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := r.Drain(ctx, opts.Target)
		if err != nil {
			if errdefs.IsValidation(err) {
				return err
			}
			if waitErr := sleepContext(ctx, r.cfg.ErrorBackoff); waitErr != nil {
				return waitErr
			}
			continue
		}
		if res.TotalClaimed() > 0 {
			continue
		}
		if err := sleepContext(ctx, r.cfg.IdleInterval); err != nil {
			return err
		}
	}
}

func (r *Runner) resolveScopes(ctx context.Context, target Target) ([]recall.Scope, error) {
	if len(target.Scopes) > 0 {
		return normalizeScopes(target.Scopes)
	}
	if target.RuntimeID == "" {
		return nil, validationf("target requires scopes or runtime_id")
	}
	if r.cfg.ScopeEnumerator == nil {
		return nil, validationf("runtime target requires ScopeEnumerator")
	}
	scopes, err := r.cfg.ScopeEnumerator.ListScopes(ctx, recall.ScopeListQuery{RuntimeID: target.RuntimeID})
	if err != nil {
		return nil, err
	}
	return normalizeScopes(scopes)
}

func normalizeScopes(scopes []recall.Scope) ([]recall.Scope, error) {
	out := make([]recall.Scope, 0, len(scopes))
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		normalized := recall.Scope{RuntimeID: scope.RuntimeID, UserID: scope.UserID}
		if normalized.PartitionKey() == "" {
			return nil, validationf("scope partition is required")
		}
		key := normalized.PartitionKey()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, normalized)
	}
	return out, nil
}

func (r *Runner) forEachScope(ctx context.Context, scopes []recall.Scope, fn func(context.Context, recall.Scope) ScopeDrainResult) []ScopeDrainResult {
	if r.cfg.MaxConcurrentScopes <= 1 || len(scopes) <= 1 {
		out := make([]ScopeDrainResult, 0, len(scopes))
		for _, scope := range scopes {
			out = append(out, fn(ctx, scope))
		}
		return out
	}
	out := make([]ScopeDrainResult, len(scopes))
	sem := make(chan struct{}, r.cfg.MaxConcurrentScopes)
	var wg sync.WaitGroup
	for i, scope := range scopes {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, scope recall.Scope) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = fn(ctx, scope)
		}(i, scope)
	}
	wg.Wait()
	return out
}

func (r *Runner) emit(ctx context.Context, event Event) {
	if r.cfg.Metrics == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = r.cfg.Now()
	}
	r.cfg.Metrics.ObserveRecallOps(ctx, event)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
