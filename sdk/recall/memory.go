package recall

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/compiler"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
	entityproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/entity"
	retrievalproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/retrieval"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// Memory is the v2 fact-centric facade. See docs §11.1.
type Memory interface {
	Save(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, error)
	Recall(ctx context.Context, scope Scope, query Query) ([]Hit, error)
	Forget(ctx context.Context, scope Scope, factID string) error
	Close() error
}

type memory struct {
	store          temporalstore.Store
	retrievalIndex retrieval.Index
	compiler       compiler.Compiler
	fanout         *projection.Fanout
	telemetry      projection.TelemetryHook
}

// New constructs a v2 Memory. The defaults wire a fully in-memory
// stack so callers can exercise the write path without external
// dependencies; production callers swap pieces in via Options.
func New(opts ...Option) (Memory, error) {
	cfg := config{
		telemetry: projection.NopTelemetry{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.store == nil {
		cfg.store = temporalstore.NewMemoryStore()
	}
	if cfg.retrievalIndex == nil {
		cfg.retrievalIndex = retrievalmem.New()
	}
	if cfg.compiler == nil {
		cfg.compiler = compiler.Default()
	}

	retrievalProj, err := retrievalproj.New(cfg.retrievalIndex)
	if err != nil {
		return nil, fmt.Errorf("recall.New: %w", err)
	}

	projections := []projection.Projection{retrievalProj, entityproj.New()}
	projections = append(projections, cfg.extraProjections...)

	return &memory{
		store:          cfg.store,
		retrievalIndex: cfg.retrievalIndex,
		compiler:       cfg.compiler,
		fanout:         projection.New(projections, cfg.telemetry),
		telemetry:      cfg.telemetry,
	}, nil
}

// Save runs the canonical write pipeline with strict transactional
// semantics:
//
//	compile -> store.Append -> fanout.ProjectRequired -> fanout.ProjectOptional
//
// If a required projection fails the call rolls back: best-effort
// fanout.ForgetRequired/Optional cleans up partially-projected
// state, then store.Delete drops the canonical fact, and the
// original projection error is returned. Cleanup failures are only
// reported through the telemetry hook so the user-visible error
// stays attributable to the original cause.
//
// Optional projections run after required ones; their failure does
// not affect the Save outcome (telemetry only).
func (m *memory) Save(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, error) {
	if err := ctx.Err(); err != nil {
		return SaveResult{}, err
	}
	if scope.RuntimeID == "" {
		return SaveResult{}, fmt.Errorf("recall.Save: scope.runtime_id is required")
	}

	result, err := m.compiler.Compile(ctx, compiler.Input{
		Scope: scope,
		Facts: req.Facts,
	})
	if err != nil {
		return SaveResult{}, err
	}
	if len(result.Facts) == 0 {
		return SaveResult{}, nil
	}

	if err := m.store.Append(ctx, result.Facts); err != nil {
		return SaveResult{}, fmt.Errorf("recall.Save: store append: %w", err)
	}

	ids := make([]string, len(result.Facts))
	for i, f := range result.Facts {
		ids[i] = f.ID
	}

	if projErr := m.fanout.ProjectRequired(ctx, result.Facts); projErr != nil {
		m.rollbackSave(ctx, scope, ids, projErr)
		return SaveResult{}, projErr
	}

	m.fanout.ProjectOptional(ctx, result.Facts)

	return SaveResult{FactIDs: ids}, nil
}

// rollbackSave best-effort undoes the canonical effects of a
// partially-completed Save. It runs both required and optional
// projection forgets (so an upstream Optional projection that
// happened to succeed before we returned does not leak), then deletes
// the canonical facts. Any failure here is reported via telemetry
// but never overrides the original projection error.
func (m *memory) rollbackSave(ctx context.Context, scope Scope, factIDs []string, cause error) {
	cleanupCtx := detachCancel(ctx)
	hook := m.fanout.Telemetry()
	if err := m.fanout.ForgetRequired(cleanupCtx, scope, factIDs); err != nil {
		hook.OnProjection(projection.ProjectionEvent{
			Projection:  "save_rollback.forget_required",
			Op:          projection.OpForget,
			Consistency: projection.Required,
			FactCount:   len(factIDs),
			Err:         fmt.Errorf("rollback cleanup after %w: %v", cause, err),
		})
	}
	m.fanout.ForgetOptional(cleanupCtx, scope, factIDs)
	if err := m.store.Delete(cleanupCtx, scope, factIDs); err != nil {
		hook.OnProjection(projection.ProjectionEvent{
			Projection:  "save_rollback.store_delete",
			Op:          projection.OpForget,
			Consistency: projection.Required,
			FactCount:   len(factIDs),
			Err:         fmt.Errorf("rollback cleanup after %w: %v", cause, err),
		})
	}
}

// Recall is wired in PR-3 alongside the planner / source / fusion /
// materialize pipeline. PR-2 keeps the surface stable but returns
// ErrNotImplemented so accidental callers fail loudly.
func (m *memory) Recall(context.Context, Scope, Query) ([]Hit, error) {
	return nil, ErrNotImplemented
}

// Forget removes a fact with strict transactional semantics:
//
//  1. Snapshot the canonical fact (used as compensation source).
//  2. fanout.ForgetRequired — on failure the canonical fact is
//     preserved so callers can retry without losing state.
//  3. store.Delete — on failure best-effort re-projects the snapshot
//     through fanout.RebuildRequired so projections do not drift
//     away from the still-present canonical fact.
//  4. fanout.ForgetOptional — best-effort, telemetry only.
//
// A missing fact id is a noop and never raises an error.
func (m *memory) Forget(ctx context.Context, scope Scope, factID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if factID == "" {
		return fmt.Errorf("recall.Forget: fact id is required")
	}

	snapshot, err := m.store.Get(ctx, scope, factID)
	if err != nil {
		if errors.Is(err, temporalstore.ErrNotFound) {
			// idempotent forget: also sweep projections in case
			// they hold drift, but treat result as success.
			_ = m.fanout.ForgetRequired(ctx, scope, []string{factID})
			m.fanout.ForgetOptional(ctx, scope, []string{factID})
			return nil
		}
		return fmt.Errorf("recall.Forget: store get: %w", err)
	}

	if err := m.fanout.ForgetRequired(ctx, scope, []string{factID}); err != nil {
		return err
	}

	if err := m.store.Delete(ctx, scope, []string{factID}); err != nil {
		m.compensateForgetStoreFailure(ctx, scope, snapshot, err)
		return fmt.Errorf("recall.Forget: store delete: %w", err)
	}

	m.fanout.ForgetOptional(ctx, scope, []string{factID})
	return nil
}

// compensateForgetStoreFailure runs after store.Delete fails between
// the projection forget and store delete steps. It tries to re-Project
// the snapshot so required projections recover the fact that still
// lives in the canonical store. The compensation is best-effort and
// only reports telemetry on failure; the user already sees the
// store-delete error returned from Forget.
func (m *memory) compensateForgetStoreFailure(ctx context.Context, scope Scope, snapshot model.TemporalFact, cause error) {
	cleanupCtx := detachCancel(ctx)
	hook := m.fanout.Telemetry()
	if err := m.fanout.ProjectRequired(cleanupCtx, []model.TemporalFact{snapshot}); err != nil {
		hook.OnProjection(projection.ProjectionEvent{
			Projection:  "forget_compensation.project_required",
			Op:          projection.OpProject,
			Consistency: projection.Required,
			FactCount:   1,
			Err:         fmt.Errorf("compensation after store delete failed %w: %v", cause, err),
		})
	}
}

// Close releases backend resources. Memory takes ownership of the
// store and retrieval index it was constructed with (whether default
// or injected): callers wiring their own backend should not also
// call Close on it.
func (m *memory) Close() error {
	if m.store != nil {
		if err := m.store.Close(); err != nil {
			return err
		}
	}
	if m.retrievalIndex != nil {
		if err := m.retrievalIndex.Close(); err != nil {
			return err
		}
	}
	return nil
}

// detachCancel returns a context that keeps the parent's values but
// is not cancelled when the parent is. Compensation paths must run
// to completion even if the inbound RPC was cancelled mid-flight,
// otherwise rollback itself becomes the source of drift.
func detachCancel(parent context.Context) context.Context {
	return cleanupCtx{parent: parent}
}

type cleanupCtx struct {
	parent context.Context
}

func (cleanupCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (cleanupCtx) Done() <-chan struct{}       { return nil }
func (cleanupCtx) Err() error                  { return nil }
func (c cleanupCtx) Value(key any) any         { return c.parent.Value(key) }
