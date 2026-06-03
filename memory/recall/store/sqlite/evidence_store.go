package sqlite

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	evidencestore "github.com/GizClaw/flowcraft/memory/recall/internal/store/evidence"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type evidenceStore struct {
	b *Backend
}

func (s *evidenceStore) Append(ctx context.Context, scope domain.Scope, factID string, refs []domain.EvidenceRef) error {
	if scope.RuntimeID == "" {
		return errdefs.Validationf("recall sqlite evidence: scope.runtime_id is required")
	}
	if factID == "" {
		return errdefs.Validationf("recall sqlite evidence: fact id is required")
	}
	if len(refs) == 0 {
		return nil
	}
	tx, err := s.b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	for i, ref := range refs {
		if ref.ID == "" {
			ref.ID = fmt.Sprintf("%s#%d", factID, i)
		}
		payload, err := sqlstmt.EncodeJSON(ref)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO recall_evidence_refs(runtime_id, user_id, fact_id, evidence_id, ordinal, payload_json)
			VALUES(?,?,?,?,?,?)
			ON CONFLICT(runtime_id, user_id, fact_id, evidence_id) DO NOTHING
		`, runtimeID, userID, factID, ref.ID, i, payload)
		if err != nil {
			return err
		}
		if rowsAffected(res) > 0 {
			continue
		}
		var existingOrdinal int
		var existingPayload string
		if err := tx.QueryRowContext(ctx, `
			SELECT ordinal, payload_json FROM recall_evidence_refs
			WHERE runtime_id = ? AND user_id = ? AND fact_id = ? AND evidence_id = ?
		`, runtimeID, userID, factID, ref.ID).Scan(&existingOrdinal, &existingPayload); err != nil {
			return err
		}
		existingRef, err := sqlstmt.DecodeJSON[domain.EvidenceRef](existingPayload)
		if err != nil {
			return err
		}
		if existingOrdinal != i || !reflect.DeepEqual(existingRef, ref) {
			return errdefs.Conflictf("recall sqlite evidence: duplicate evidence id %q for fact %q with different payload", ref.ID, factID)
		}
	}
	return tx.Commit()
}

func (s *evidenceStore) Get(ctx context.Context, scope domain.Scope, evidenceID string) (domain.EvidenceRef, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	rows, err := s.b.db.QueryContext(ctx, `
		SELECT payload_json FROM recall_evidence_refs
		WHERE runtime_id = ? AND user_id = ? AND evidence_id = ?
		ORDER BY fact_id ASC
		LIMIT 2
	`, runtimeID, userID, evidenceID)
	if err != nil {
		return domain.EvidenceRef{}, err
	}
	defer rows.Close()
	var payloads []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return domain.EvidenceRef{}, err
		}
		payloads = append(payloads, payload)
	}
	if err := rows.Err(); err != nil {
		return domain.EvidenceRef{}, err
	}
	if len(payloads) == 0 {
		return domain.EvidenceRef{}, evidencestore.ErrNotFound
	}
	if len(payloads) > 1 {
		return domain.EvidenceRef{}, evidencestore.ErrAmbiguous
	}
	payload := payloads[0]
	return sqlstmt.DecodeJSON[domain.EvidenceRef](payload)
}

func (s *evidenceStore) ListByFact(ctx context.Context, scope domain.Scope, factID string) ([]domain.EvidenceRef, error) {
	if factID == "" {
		return nil, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	rows, err := s.b.db.QueryContext(ctx, `
		SELECT payload_json FROM recall_evidence_refs
		WHERE runtime_id = ? AND user_id = ? AND fact_id = ?
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
	rows, err := s.b.db.QueryContext(ctx, `
		SELECT DISTINCT fact_id FROM recall_evidence_refs
		WHERE runtime_id = ? AND user_id = ?
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
	_, err := s.b.db.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM recall_evidence_refs
		WHERE runtime_id = ? AND user_id = ? AND fact_id IN (%s)
	`, phs(3, len(ids))), args...)
	return err
}

func (s *evidenceStore) Close() error { return nil }

var _ port.EvidenceStore = (*evidenceStore)(nil)
