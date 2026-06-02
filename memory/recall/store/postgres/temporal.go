package postgres

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/jackc/pgx/v5"
)

type temporalStore struct {
	b *Backend
}

func (s *temporalStore) Append(ctx context.Context, facts []domain.TemporalFact) error {
	if len(facts) == 0 {
		return nil
	}
	staged := make([]domain.TemporalFact, 0, len(facts))
	seen := make(map[string]struct{}, len(facts))
	for _, f := range facts {
		if f.ID == "" {
			return errdefs.Validationf("recall postgres temporal: fact id is required")
		}
		if !f.Kind.IsValid() {
			return errdefs.Validationf("recall postgres temporal: invalid fact kind %q for fact %q", f.Kind, f.ID)
		}
		if f.Scope.RuntimeID == "" {
			return errdefs.Validationf("recall postgres temporal: fact %q missing scope.runtime_id", f.ID)
		}
		k := f.Scope.PartitionKey() + "/" + f.ID
		if _, ok := seen[k]; ok {
			return errdefs.Conflictf("recall postgres temporal: duplicate fact id %q within append batch", f.ID)
		}
		seen[k] = struct{}{}
		staged = append(staged, f.Clone())
	}

	tx, err := s.b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, f := range staged {
		exists, err := s.factExistsTx(ctx, tx, f.Scope, f.ID)
		if err != nil {
			return err
		}
		if exists {
			return errdefs.Conflictf("recall postgres temporal: duplicate fact id %q in scope", f.ID)
		}
		if err := s.insertFactTx(ctx, tx, f); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *temporalStore) Get(ctx context.Context, scope domain.Scope, factID string) (domain.TemporalFact, error) {
	f, err := s.loadFact(ctx, scope, factID)
	if err != nil {
		return domain.TemporalFact{}, err
	}
	return f.Clone(), nil
}

func (s *temporalStore) List(ctx context.Context, scope domain.Scope, query port.ListQuery) ([]domain.TemporalFact, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	args := []any{runtimeID, userID}
	stmt := `SELECT payload_json FROM recall_facts WHERE runtime_id = $1 AND user_id = $2`
	entities := sqlstmt.UniqueNonEmpty(query.Entities)
	if len(entities) > 0 {
		start := len(args) + 1
		args = append(args, runtimeID, userID)
		for _, entity := range entities {
			args = append(args, entity)
		}
		args = append(args, len(entities))
		stmt += fmt.Sprintf(` AND id IN (
			SELECT fact_id FROM recall_fact_entities
			WHERE runtime_id = %s AND user_id = %s AND entity IN (%s)
			GROUP BY fact_id
			HAVING COUNT(DISTINCT entity) = %s
		)`, ph(start), ph(start+1), phs(start+2, len(entities)), ph(start+2+len(entities)))
	}
	stmt += ` ORDER BY observed_at_ns ASC, id ASC`
	rows, err := s.b.pool.Query(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	kindSet := make(map[domain.FactKind]struct{}, len(query.Kinds))
	for _, k := range query.Kinds {
		kindSet[k] = struct{}{}
	}
	out := make([]domain.TemporalFact, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		f, err := sqlstmt.DecodeJSON[domain.TemporalFact](raw)
		if err != nil {
			return nil, err
		}
		if !query.IncludeSuperseded && f.CorrectedBy != "" {
			continue
		}
		if len(kindSet) > 0 {
			if _, ok := kindSet[f.Kind]; !ok {
				continue
			}
		}
		out = append(out, f.Clone())
		if query.Limit > 0 && len(out) >= query.Limit {
			break
		}
	}
	return out, rows.Err()
}

func (s *temporalStore) FindByMergeKey(ctx context.Context, scope domain.Scope, mergeKey string) ([]domain.TemporalFact, error) {
	if mergeKey == "" {
		return nil, nil
	}
	return s.findFacts(ctx, scope, "merge_key", mergeKey)
}

func (s *temporalStore) FindSupersededBy(ctx context.Context, scope domain.Scope, factID string) ([]domain.TemporalFact, error) {
	if factID == "" {
		return nil, nil
	}
	return s.findFacts(ctx, scope, "corrected_by", factID)
}

func (s *temporalStore) FindByOriginRequestID(ctx context.Context, scope domain.Scope, requestID string) ([]domain.TemporalFact, error) {
	if requestID == "" {
		return nil, nil
	}
	return s.findFacts(ctx, scope, "origin_request_id", requestID)
}

func (s *temporalStore) FindByRevisionSource(ctx context.Context, scope domain.Scope, sourceFactID string) ([]domain.TemporalFact, error) {
	if sourceFactID == "" {
		return nil, nil
	}
	facts, err := s.List(ctx, scope, port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		return nil, err
	}
	out := make([]domain.TemporalFact, 0)
	for _, f := range facts {
		rev, ok := domain.RevisionOf(f)
		if ok && rev.SourceFactID == sourceFactID {
			out = append(out, f.Clone())
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ObservedAt.Before(out[j].ObservedAt) })
	return out, nil
}

func (s *temporalStore) UpdateValidity(ctx context.Context, scope domain.Scope, factID string, validTo time.Time, correctedBy string) error {
	return s.updateFact(ctx, scope, factID, func(f *domain.TemporalFact) error {
		if f.ValidTo != nil {
			if f.ValidTo.Equal(validTo) && f.CorrectedBy == correctedBy {
				return nil
			}
			return temporalstore.ErrValidityAlreadyClosed
		}
		vt := validTo
		f.ValidTo = &vt
		f.CorrectedBy = correctedBy
		return nil
	})
}

func (s *temporalStore) ReopenValidity(ctx context.Context, scope domain.Scope, factID string, expectedCorrectedBy string) error {
	return s.updateFact(ctx, scope, factID, func(f *domain.TemporalFact) error {
		if f.ValidTo == nil && f.CorrectedBy == "" {
			return nil
		}
		if f.CorrectedBy != expectedCorrectedBy {
			return temporalstore.ErrReopenConflict
		}
		f.ValidTo = nil
		f.CorrectedBy = ""
		return nil
	})
}

func (s *temporalStore) Delete(ctx context.Context, scope domain.Scope, factIDs []string) error {
	if len(factIDs) == 0 {
		return nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	tx, err := s.b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, id := range factIDs {
		if id == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM recall_fact_entities WHERE runtime_id = $1 AND user_id = $2 AND fact_id = $3`, runtimeID, userID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM recall_facts WHERE runtime_id = $1 AND user_id = $2 AND id = $3`, runtimeID, userID, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *temporalStore) UpdateFeedback(ctx context.Context, scope domain.Scope, factID string, reinforcementDelta, penaltyDelta float64) error {
	return s.updateFact(ctx, scope, factID, func(f *domain.TemporalFact) error {
		f.Reinforcement = sqlstmt.ClampNonNeg(f.Reinforcement + reinforcementDelta)
		f.Penalty = sqlstmt.ClampNonNeg(f.Penalty + penaltyDelta)
		return nil
	})
}

func (s *temporalStore) MarkClosed(ctx context.Context, scope domain.Scope, factID string, closed bool) error {
	return s.updateFact(ctx, scope, factID, func(f *domain.TemporalFact) error {
		f.Closed = closed
		return nil
	})
}

func (s *temporalStore) ListByID(ctx context.Context, scope domain.Scope, factID string) ([]domain.TemporalFact, error) {
	seed, err := s.Get(ctx, scope, factID)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{seed.ID: {}}
	out := []domain.TemporalFact{seed}
	queue := []string{seed.ID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		f, err := s.Get(ctx, scope, id)
		if err != nil {
			continue
		}
		for _, prior := range f.Supersedes {
			if _, ok := seen[prior]; prior == "" || ok {
				continue
			}
			seen[prior] = struct{}{}
			pf, err := s.Get(ctx, scope, prior)
			if err == nil {
				out = append(out, pf)
				queue = append(queue, prior)
			}
		}
		successors, err := s.FindSupersededBy(ctx, scope, id)
		if err != nil {
			continue
		}
		for _, succ := range successors {
			if _, ok := seen[succ.ID]; ok {
				continue
			}
			seen[succ.ID] = struct{}{}
			out = append(out, succ)
			queue = append(queue, succ.ID)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ObservedAt.Before(out[j].ObservedAt) })
	return out, nil
}

func (s *temporalStore) DeleteByScope(ctx context.Context, scope domain.Scope) (int, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	tx, err := s.b.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM recall_facts WHERE runtime_id = $1 AND user_id = $2`, runtimeID, userID).Scan(&n); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM recall_fact_entities WHERE runtime_id = $1 AND user_id = $2`, runtimeID, userID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM recall_facts WHERE runtime_id = $1 AND user_id = $2`, runtimeID, userID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *temporalStore) ListScopes(ctx context.Context, query port.ScopeListQuery) ([]domain.Scope, error) {
	var rows pgx.Rows
	var err error
	if query.RuntimeID == "" {
		rows, err = s.b.pool.Query(ctx, `SELECT DISTINCT runtime_id, user_id FROM recall_facts ORDER BY runtime_id ASC, user_id ASC`)
	} else {
		rows, err = s.b.pool.Query(ctx, `SELECT DISTINCT runtime_id, user_id FROM recall_facts WHERE runtime_id = $1 ORDER BY runtime_id ASC, user_id ASC`, query.RuntimeID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scopes []domain.Scope
	for rows.Next() {
		var runtimeID, userID string
		if err := rows.Scan(&runtimeID, &userID); err != nil {
			return nil, err
		}
		scopes = append(scopes, sqlstmt.ScopeFromParts(runtimeID, userID))
	}
	return scopes, rows.Err()
}

func (s *temporalStore) Close() error { return s.b.Close() }

func (s *temporalStore) factExistsTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, id string) (bool, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM recall_facts WHERE runtime_id = $1 AND user_id = $2 AND id = $3`, runtimeID, userID, id).Scan(&one)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *temporalStore) insertFactTx(ctx context.Context, tx pgx.Tx, f domain.TemporalFact) error {
	raw, err := sqlstmt.EncodeJSON(f)
	if err != nil {
		return err
	}
	runtimeID, userID := sqlstmt.ScopeParts(f.Scope)
	_, err = tx.Exec(ctx,
		fmt.Sprintf(`INSERT INTO recall_facts(runtime_id, user_id, id, kind, observed_at_ns, valid_to_ns, closed, expires_at_ns, merge_key, corrected_by, origin_request_id, payload_json) VALUES(%s)`, phs(1, 12)),
		runtimeID, userID, f.ID, string(f.Kind), f.ObservedAt.UnixNano(), sqlstmt.TimePtrNS(f.ValidTo),
		sqlstmt.BoolInt(f.Closed), sqlstmt.TimePtrNS(f.ExpiresAt), f.MergeKey, f.CorrectedBy, f.Origin.RequestID, raw)
	if err != nil {
		return err
	}
	return s.replaceEntityRowsTx(ctx, tx, f)
}

func (s *temporalStore) replaceEntityRowsTx(ctx context.Context, tx pgx.Tx, f domain.TemporalFact) error {
	runtimeID, userID := sqlstmt.ScopeParts(f.Scope)
	if _, err := tx.Exec(ctx, `DELETE FROM recall_fact_entities WHERE runtime_id = $1 AND user_id = $2 AND fact_id = $3`, runtimeID, userID, f.ID); err != nil {
		return err
	}
	for _, entity := range sqlstmt.UniqueNonEmpty(f.Entities) {
		if _, err := tx.Exec(ctx, `INSERT INTO recall_fact_entities(runtime_id, user_id, fact_id, entity) VALUES($1,$2,$3,$4)`, runtimeID, userID, f.ID, entity); err != nil {
			return err
		}
	}
	return nil
}

func (s *temporalStore) loadFact(ctx context.Context, scope domain.Scope, factID string) (domain.TemporalFact, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	var raw string
	err := s.b.pool.QueryRow(ctx, `SELECT payload_json FROM recall_facts WHERE runtime_id = $1 AND user_id = $2 AND id = $3`, runtimeID, userID, factID).Scan(&raw)
	if err == pgx.ErrNoRows {
		return domain.TemporalFact{}, temporalstore.ErrNotFound
	}
	if err != nil {
		return domain.TemporalFact{}, err
	}
	return sqlstmt.DecodeJSON[domain.TemporalFact](raw)
}

func (s *temporalStore) loadFactTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, factID string) (domain.TemporalFact, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	var raw string
	err := tx.QueryRow(ctx, `SELECT payload_json FROM recall_facts WHERE runtime_id = $1 AND user_id = $2 AND id = $3 FOR UPDATE`, runtimeID, userID, factID).Scan(&raw)
	if err == pgx.ErrNoRows {
		return domain.TemporalFact{}, temporalstore.ErrNotFound
	}
	if err != nil {
		return domain.TemporalFact{}, err
	}
	return sqlstmt.DecodeJSON[domain.TemporalFact](raw)
}

func (s *temporalStore) updateFact(ctx context.Context, scope domain.Scope, factID string, mutate func(*domain.TemporalFact) error) error {
	tx, err := s.b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	f, err := s.loadFactTx(ctx, tx, scope, factID)
	if err != nil {
		return err
	}
	if err := mutate(&f); err != nil {
		return err
	}
	raw, err := sqlstmt.EncodeJSON(f)
	if err != nil {
		return err
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	if _, err := tx.Exec(ctx,
		`UPDATE recall_facts SET valid_to_ns = $1, closed = $2, expires_at_ns = $3, corrected_by = $4, origin_request_id = $5, payload_json = $6 WHERE runtime_id = $7 AND user_id = $8 AND id = $9`,
		sqlstmt.TimePtrNS(f.ValidTo), sqlstmt.BoolInt(f.Closed), sqlstmt.TimePtrNS(f.ExpiresAt), f.CorrectedBy, f.Origin.RequestID, raw,
		runtimeID, userID, factID); err != nil {
		return err
	}
	if err := s.replaceEntityRowsTx(ctx, tx, f); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *temporalStore) findFacts(ctx context.Context, scope domain.Scope, col, value string) ([]domain.TemporalFact, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	rows, err := s.b.pool.Query(ctx,
		fmt.Sprintf(`SELECT payload_json FROM recall_facts WHERE runtime_id = $1 AND user_id = $2 AND %s = $3 ORDER BY observed_at_ns ASC, id ASC`, col),
		runtimeID, userID, value)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.TemporalFact, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		f, err := sqlstmt.DecodeJSON[domain.TemporalFact](raw)
		if err != nil {
			return nil, err
		}
		out = append(out, f.Clone())
	}
	return out, rows.Err()
}

var _ port.TemporalStore = (*temporalStore)(nil)
var _ port.ScopeEnumerator = (*temporalStore)(nil)
