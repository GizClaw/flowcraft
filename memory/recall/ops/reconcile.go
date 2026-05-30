package ops

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall"
)

// ReconcileRuntime runs namespace-wide TTL sweep and side-effect repair using
// the Runner's default reconcile options plus any explicit overrides.
func (r *Runner) ReconcileRuntime(ctx context.Context, runtimeID string, opts ...recall.ReconcileOptions) (recall.ReconcileResult, error) {
	if r.reconcile == nil {
		return recall.ReconcileResult{}, validationf("memory does not expose Reconciler")
	}
	if runtimeID == "" {
		return recall.ReconcileResult{}, validationf("runtime_id is required")
	}
	reconcileOpts := r.reconcileOptions(opts...)
	started := r.cfg.Now()
	res, err := r.reconcile.ReconcileRuntime(ctx, runtimeID, reconcileOpts)
	r.emit(ctx, Event{
		Time:      r.cfg.Now(),
		Kind:      EventReconcile,
		RuntimeID: runtimeID,
		Duration:  r.cfg.Now().Sub(started),
		Err:       errString(err),
		Reconcile: &res,
	})
	return res, err
}

// ReconcileScopes runs TTL sweep and side-effect repair for explicit scopes.
func (r *Runner) ReconcileScopes(ctx context.Context, scopes []recall.Scope, opts ...recall.ReconcileOptions) (recall.ReconcileResult, error) {
	if r.reconcile == nil {
		return recall.ReconcileResult{}, validationf("memory does not expose Reconciler")
	}
	scopes, err := normalizeScopes(scopes)
	if err != nil {
		return recall.ReconcileResult{}, err
	}
	reconcileOpts := r.reconcileOptions(opts...)
	started := r.cfg.Now()
	res, err := r.reconcile.ReconcileScopes(ctx, scopes, reconcileOpts)
	r.emit(ctx, Event{
		Time:      r.cfg.Now(),
		Kind:      EventReconcile,
		Duration:  r.cfg.Now().Sub(started),
		Err:       errString(err),
		Reconcile: &res,
	})
	return res, err
}

// ReconcileAsyncSemantic audits semantic derivation state for one scope.
func (r *Runner) ReconcileAsyncSemantic(ctx context.Context, scope recall.Scope, opts recall.AsyncSemanticReconcileOptions) (recall.AsyncSemanticReconcileResult, error) {
	if r.reconcile == nil {
		return recall.AsyncSemanticReconcileResult{}, validationf("memory does not expose Reconciler")
	}
	if opts.Now.IsZero() {
		opts.Now = r.cfg.Now()
	}
	return r.reconcile.ReconcileAsyncSemantic(ctx, scope, opts)
}

// ReconcileSideEffects repairs projections for one scope.
func (r *Runner) ReconcileSideEffects(ctx context.Context, scope recall.Scope, opts recall.SideEffectReconcileOptions) (recall.SideEffectReconcileResult, error) {
	if r.reconcile == nil {
		return recall.SideEffectReconcileResult{}, validationf("memory does not expose Reconciler")
	}
	return r.reconcile.ReconcileSideEffects(ctx, scope, opts)
}

func (r *Runner) reconcileOptions(overrides ...recall.ReconcileOptions) recall.ReconcileOptions {
	opts := r.cfg.ReconcileOptions
	if len(overrides) > 0 {
		opts = overrides[len(overrides)-1]
	}
	if opts.Now.IsZero() {
		opts.Now = r.cfg.Now()
	}
	if opts.Enumerator == nil && r.cfg.ScopeEnumerator != nil {
		opts.Enumerator = r.cfg.ScopeEnumerator
	}
	return opts
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
