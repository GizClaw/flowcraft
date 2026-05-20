package recall

import (
	"context"
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

// ErrScopeKeyMismatch is returned when ForgetAll's confirmScopeKey does
// not match scope.CanonicalKey() (GDPR guard).
var ErrScopeKeyMismatch = errdefs.Forbidden(errdefs.New("recall: scope key confirmation mismatch"))

// Forget removes a fact. Optional mode defaults to ForgetHard for backward
// compatibility. Use ForgetSoft to retract without deleting audit history
// (equivalent to v1 Retract / D.1 guidance).
func (m *memory) Forget(ctx context.Context, scope Scope, factID string, mode ...ForgetMode) error {
	mmode := domain.ForgetHard
	if len(mode) > 0 {
		mmode = domain.NormalizeForgetMode(mode[0])
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if factID == "" {
		return errdefs.Validationf("recall.Forget: fact id is required")
	}

	unlock := m.lockWriteScope(scope)
	defer unlock()

	snapshot, err := m.store.Get(ctx, scope, factID)
	if err != nil {
		if errors.Is(err, temporalstore.ErrNotFound) {
			if mmode == domain.ForgetHard {
				_ = m.fanout.ForgetRequired(ctx, scope, []string{factID})
				m.fanout.ForgetOptional(ctx, scope, []string{factID})
			}
			return nil
		}
		return fmt.Errorf("recall.Forget: store get: %w", err)
	}

	switch mmode {
	case domain.ForgetSoft:
		if err := m.store.MarkClosed(ctx, scope, factID, true); err != nil {
			return fmt.Errorf("recall.Forget: soft close: %w", err)
		}
		snapshot.Closed = true
		if err := m.fanout.ProjectRequired(ctx, []domain.TemporalFact{snapshot}); err != nil {
			return fmt.Errorf("recall.Forget: reproject closed fact: %w", err)
		}
		m.fanout.ProjectOptional(ctx, []domain.TemporalFact{snapshot})
		return nil
	default:
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
}

// ForgetAll applies Soft or Hard retirement to every fact in the primary
// scope only (Federation sub-scopes are not affected). Hard mode is
// irreversible; confirmScopeKey must equal scope.CanonicalKey().
func (m *memory) ForgetAll(ctx context.Context, scope Scope, mode ForgetMode, confirmScopeKey string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if scope.RuntimeID == "" {
		return 0, errdefs.Validationf("recall.ForgetAll: scope.runtime_id is required")
	}
	mmode := domain.NormalizeForgetMode(mode)
	if mmode == domain.ForgetHard && confirmScopeKey != scope.CanonicalKey() {
		return 0, ErrScopeKeyMismatch
	}

	unlock := m.lockWriteScope(scope)
	defer unlock()

	facts, err := m.store.List(ctx, scope, port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		return 0, fmt.Errorf("recall.ForgetAll: list: %w", err)
	}
	if len(facts) == 0 {
		return 0, nil
	}

	switch mmode {
	case domain.ForgetSoft:
		for _, f := range facts {
			if err := m.store.MarkClosed(ctx, scope, f.ID, true); err != nil {
				return 0, err
			}
		}
		closed := make([]domain.TemporalFact, len(facts))
		for i, f := range facts {
			f.Closed = true
			closed[i] = f
		}
		if err := m.fanout.ProjectRequired(ctx, closed); err != nil {
			return 0, err
		}
		m.fanout.ProjectOptional(ctx, closed)
		return len(facts), nil
	default:
		ids := make([]string, 0, len(facts))
		for _, f := range facts {
			ids = append(ids, f.ID)
		}
		if err := m.fanout.ForgetRequired(ctx, scope, ids); err != nil {
			return 0, err
		}
		n, err := m.store.DeleteByScope(ctx, scope)
		if err != nil {
			return 0, fmt.Errorf("recall.ForgetAll: store delete: %w", err)
		}
		m.fanout.ForgetOptional(ctx, scope, ids)
		return n, nil
	}
}
