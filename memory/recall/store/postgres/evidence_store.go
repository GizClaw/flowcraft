package postgres

import (
	"context"
	"fmt"
	"sort"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	evidencestore "github.com/GizClaw/flowcraft/memory/recall/internal/store/evidence"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/jackc/pgx/v5"
)

type evidenceStore struct {
	b *Backend
}

func (s *evidenceStore) Append(ctx context.Context, scope domain.Scope, factID string, refs []domain.EvidenceRef) error {
	if scope.RuntimeID == "" {
		return errdefs.Validationf("recall postgres evidence: scope.runtime_id is required")
	}
	if factID == "" {
		return errdefs.Validationf("recall postgres evidence: fact id is required")
	}
	if len(refs) == 0 {
		return nil
	}
	tx, err := s.b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	for i, ref := range refs {
		if ref.ID == "" {
			ref.ID = fmt.Sprintf("%s#%d", factID, i)
		}
		payload, err := sqlstmt.EncodeJSON(ref)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO recall_evidence_refs(runtime_id, user_id, fact_id, evidence_id, ordinal, payload_json)
			VALUES($1,$2,$3,$4,$5,$6)
			ON CONFLICT(runtime_id, user_id, fact_id, evidence_id) DO UPDATE SET
				ordinal = excluded.ordinal,
				payload_json = excluded.payload_json
		`, runtimeID, userID, factID, ref.ID, i, payload); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *evidenceStore) Get(ctx context.Context, scope domain.Scope, evidenceID string) (domain.EvidenceRef, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	var payload string
	err := s.b.pool.QueryRow(ctx, `
		SELECT payload_json FROM recall_evidence_refs
		WHERE runtime_id = $1 AND user_id = $2 AND evidence_id = $3
		ORDER BY fact_id ASC
	`, runtimeID, userID, evidenceID).Scan(&payload)
	if err != nil {
		if err == pgx.ErrNoRows {
			return domain.EvidenceRef{}, evidencestore.ErrNotFound
		}
		return domain.EvidenceRef{}, err
	}
	return sqlstmt.DecodeJSON[domain.EvidenceRef](payload)
}

func (s *evidenceStore) ListByFact(ctx context.Context, scope domain.Scope, factID string) ([]domain.EvidenceRef, error) {
	if factID == "" {
		return nil, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	rows, err := s.b.pool.Query(ctx, `
		SELECT payload_json FROM recall_evidence_refs
		WHERE runtime_id = $1 AND user_id = $2 AND fact_id = $3
		ORDER BY ordinal ASC, evidence_id ASC
	`, runtimeID, userID, factID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EvidenceRef
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		ref, err := sqlstmt.DecodeJSON[domain.EvidenceRef](payload)
		if err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

func (s *evidenceStore) ListFactIDs(ctx context.Context, scope domain.Scope) ([]string, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	rows, err := s.b.pool.Query(ctx, `
		SELECT DISTINCT fact_id FROM recall_evidence_refs
		WHERE runtime_id = $1 AND user_id = $2
	`, runtimeID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var factID string
		if err := rows.Scan(&factID); err != nil {
			return nil, err
		}
		out = append(out, factID)
	}
	sort.Strings(out)
	return out, rows.Err()
}

func (s *evidenceStore) ForgetByFact(ctx context.Context, scope domain.Scope, factIDs []string) error {
	ids := sqlstmt.UniqueNonEmpty(factIDs)
	if len(ids) == 0 {
		return nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	args := make([]any, 0, 2+len(ids))
	args = append(args, runtimeID, userID)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.b.pool.Exec(ctx, fmt.Sprintf(`
		DELETE FROM recall_evidence_refs
		WHERE runtime_id = $1 AND user_id = $2 AND fact_id IN (%s)
	`, phs(3, len(ids))), args...)
	return err
}

func (s *evidenceStore) Close() error { return s.b.Close() }

var _ port.EvidenceStore = (*evidenceStore)(nil)
