package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type observationStore struct {
	b *Backend
}

func (s *observationStore) Append(ctx context.Context, observations []domain.Observation) error {
	if len(observations) == 0 {
		return nil
	}
	tx, err := s.b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, obs := range observations {
		if obs.ID == "" {
			return errdefs.Validationf("recall sqlite observation: observation id is required")
		}
		if obs.Scope.RuntimeID == "" {
			return errdefs.Validationf("recall sqlite observation: observation %q missing scope.runtime_id", obs.ID)
		}
		runtimeID, userID := sqlstmt.ScopeParts(obs.Scope)
		payload, err := sqlstmt.EncodeJSON(obs)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO recall_observations(runtime_id, user_id, id, kind, source_id, observed_at_ns, payload_json)
			VALUES(?,?,?,?,?,?,?)
		`, runtimeID, userID, obs.ID, string(obs.Kind), obs.SourceID, obs.ObservedAt.UnixNano(), payload)
		if err != nil {
			return err
		}
		if rowsAffected(res) > 0 {
			continue
		}
		var existingPayload string
		err = tx.QueryRowContext(ctx, `
			SELECT payload_json FROM recall_observations
			WHERE runtime_id = ? AND user_id = ? AND id = ?
		`, runtimeID, userID, obs.ID).Scan(&existingPayload)
		if err != nil {
			return err
		}
		existing, err := sqlstmt.DecodeJSON[domain.Observation](existingPayload)
		if err != nil {
			return err
		}
		merged, changed, conflict := domain.MergeObservation(existing, obs)
		if conflict {
			return errdefs.Conflictf("recall sqlite observation: duplicate observation id %q in scope", obs.ID)
		}
		if !changed {
			continue
		}
		payload, err = sqlstmt.EncodeJSON(merged)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE recall_observations
			SET kind = ?, source_id = ?, observed_at_ns = ?, payload_json = ?
			WHERE runtime_id = ? AND user_id = ? AND id = ?
		`, string(merged.Kind), merged.SourceID, merged.ObservedAt.UnixNano(), payload, runtimeID, userID, obs.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *observationStore) Get(ctx context.Context, scope domain.Scope, observationID string) (domain.Observation, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	var payload string
	err := s.b.db.QueryRowContext(ctx, `
		SELECT payload_json FROM recall_observations
		WHERE runtime_id = ? AND user_id = ? AND id = ?
	`, runtimeID, userID, observationID).Scan(&payload)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.Observation{}, port.ErrNotFound
		}
		return domain.Observation{}, err
	}
	return sqlstmt.DecodeJSON[domain.Observation](payload)
}

func (s *observationStore) List(ctx context.Context, scope domain.Scope, query port.ObservationListQuery) ([]domain.Observation, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	sqlText, args := sqliteObservationListSQL(runtimeID, userID, query)
	rows, err := s.b.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanObservations(rows)
}

func (s *observationStore) Delete(ctx context.Context, scope domain.Scope, observationIDs []string) error {
	ids := sqlstmt.UniqueNonEmpty(observationIDs)
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
		DELETE FROM recall_observations
		WHERE runtime_id = ? AND user_id = ? AND id IN (%s)
	`, phs(3, len(ids))), args...)
	return err
}

func (s *observationStore) DeleteByScope(ctx context.Context, scope domain.Scope) (int, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	res, err := s.b.db.ExecContext(ctx, `
		DELETE FROM recall_observations WHERE runtime_id = ? AND user_id = ?
	`, runtimeID, userID)
	if err != nil {
		return 0, err
	}
	return rowsAffected(res), nil
}

func (s *observationStore) Close() error { return s.b.Close() }

type linkStore struct {
	b *Backend
}

func (s *linkStore) Append(ctx context.Context, links []domain.FactLink) error {
	if len(links) == 0 {
		return nil
	}
	tx, err := s.b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, link := range links {
		if err := validateLink(link, "sqlite"); err != nil {
			return err
		}
		payload, err := sqlstmt.EncodeJSON(link)
		if err != nil {
			return err
		}
		runtimeID, userID := sqlstmt.ScopeParts(link.Scope)
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO recall_links(runtime_id, user_id, id, type, from_kind, from_id, to_kind, to_id, merge_key, created_at_ns, payload_json)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)
		`, runtimeID, userID, link.ID, string(link.Type), string(link.From.Kind), link.From.ID, string(link.To.Kind), link.To.ID, link.MergeKey, link.CreatedAt.UnixNano(), payload); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *linkStore) Get(ctx context.Context, scope domain.Scope, linkID string) (domain.FactLink, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	var payload string
	err := s.b.db.QueryRowContext(ctx, `
		SELECT payload_json FROM recall_links
		WHERE runtime_id = ? AND user_id = ? AND id = ?
	`, runtimeID, userID, linkID).Scan(&payload)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.FactLink{}, port.ErrNotFound
		}
		return domain.FactLink{}, err
	}
	return sqlstmt.DecodeJSON[domain.FactLink](payload)
}

func (s *linkStore) List(ctx context.Context, scope domain.Scope, query port.LinkListQuery) ([]domain.FactLink, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	rows, err := s.b.db.QueryContext(ctx, `
		SELECT payload_json FROM recall_links
		WHERE runtime_id = ? AND user_id = ?
		ORDER BY created_at_ns ASC, id ASC
	`, runtimeID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanLinks(rows)
	if err != nil {
		return nil, err
	}
	out = filterLinks(out, query)
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (s *linkStore) FindByNode(ctx context.Context, scope domain.Scope, node domain.GraphNodeRef) ([]domain.FactLink, error) {
	if node.Kind == "" || node.ID == "" {
		return nil, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	rows, err := s.b.db.QueryContext(ctx, `
		SELECT payload_json FROM recall_links
		WHERE runtime_id = ? AND user_id = ?
		  AND ((from_kind = ? AND from_id = ?) OR (to_kind = ? AND to_id = ?))
		ORDER BY created_at_ns ASC, id ASC
	`, runtimeID, userID, string(node.Kind), node.ID, string(node.Kind), node.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLinks(rows)
}

func (s *linkStore) FindByMergeKey(ctx context.Context, scope domain.Scope, mergeKey string) ([]domain.FactLink, error) {
	if mergeKey == "" {
		return nil, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	var payload string
	err := s.b.db.QueryRowContext(ctx, `
		SELECT payload_json FROM recall_links
		WHERE runtime_id = ? AND user_id = ? AND merge_key = ?
	`, runtimeID, userID, mergeKey).Scan(&payload)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	link, err := sqlstmt.DecodeJSON[domain.FactLink](payload)
	if err != nil {
		return nil, err
	}
	return []domain.FactLink{link}, nil
}

func (s *linkStore) Delete(ctx context.Context, scope domain.Scope, linkIDs []string) error {
	ids := sqlstmt.UniqueNonEmpty(linkIDs)
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
		DELETE FROM recall_links
		WHERE runtime_id = ? AND user_id = ? AND id IN (%s)
	`, phs(3, len(ids))), args...)
	return err
}

func (s *linkStore) DeleteByNode(ctx context.Context, scope domain.Scope, node domain.GraphNodeRef) (int, error) {
	if node.Kind == "" || node.ID == "" {
		return 0, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	res, err := s.b.db.ExecContext(ctx, `
		DELETE FROM recall_links
		WHERE runtime_id = ? AND user_id = ?
		  AND ((from_kind = ? AND from_id = ?) OR (to_kind = ? AND to_id = ?))
	`, runtimeID, userID, string(node.Kind), node.ID, string(node.Kind), node.ID)
	if err != nil {
		return 0, err
	}
	return rowsAffected(res), nil
}

func (s *linkStore) DeleteByScope(ctx context.Context, scope domain.Scope) (int, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	res, err := s.b.db.ExecContext(ctx, `
		DELETE FROM recall_links WHERE runtime_id = ? AND user_id = ?
	`, runtimeID, userID)
	if err != nil {
		return 0, err
	}
	return rowsAffected(res), nil
}

func (s *linkStore) Close() error { return s.b.Close() }

func scanObservations(rows *sql.Rows) ([]domain.Observation, error) {
	var out []domain.Observation
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		obs, err := sqlstmt.DecodeJSON[domain.Observation](payload)
		if err != nil {
			return nil, err
		}
		out = append(out, obs)
	}
	return out, rows.Err()
}

func scanLinks(rows *sql.Rows) ([]domain.FactLink, error) {
	var out []domain.FactLink
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		link, err := sqlstmt.DecodeJSON[domain.FactLink](payload)
		if err != nil {
			return nil, err
		}
		out = append(out, link)
	}
	return out, rows.Err()
}

func sqliteObservationListSQL(runtimeID, userID string, query port.ObservationListQuery) (string, []any) {
	args := []any{runtimeID, userID}
	sqlText := `
		SELECT payload_json FROM recall_observations
		WHERE runtime_id = ? AND user_id = ?
	`
	if len(query.Kinds) > 0 {
		sqlText += fmt.Sprintf(" AND kind IN (%s)", phs(len(args)+1, len(query.Kinds)))
		for _, kind := range query.Kinds {
			args = append(args, string(kind))
		}
	}
	if query.SourceID != "" {
		sqlText += " AND source_id = ?"
		args = append(args, query.SourceID)
	}
	sqlText += " ORDER BY observed_at_ns ASC, id ASC"
	if query.Limit > 0 {
		sqlText += " LIMIT ?"
		args = append(args, query.Limit)
	}
	return sqlText, args
}

func filterLinks(links []domain.FactLink, query port.LinkListQuery) []domain.FactLink {
	typeSet := make(map[domain.FactLinkType]struct{}, len(query.Types))
	for _, typ := range query.Types {
		typeSet[typ] = struct{}{}
	}
	out := links[:0]
	for _, link := range links {
		if len(typeSet) > 0 {
			if _, ok := typeSet[link.Type]; !ok {
				continue
			}
		}
		if !nodeRefMatches(query.From, link.From) || !nodeRefMatches(query.To, link.To) {
			continue
		}
		out = append(out, link)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func rowsAffected(res sql.Result) int {
	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return int(n)
}

func nodeRefMatches(query, actual domain.GraphNodeRef) bool {
	if query.Kind != "" && query.Kind != actual.Kind {
		return false
	}
	if query.ID != "" && query.ID != actual.ID {
		return false
	}
	return true
}

func validateLink(link domain.FactLink, backend string) error {
	if link.ID == "" {
		return errdefs.Validationf("recall %s link: link id is required", backend)
	}
	if link.Scope.RuntimeID == "" {
		return errdefs.Validationf("recall %s link: link %q missing scope.runtime_id", backend, link.ID)
	}
	if link.Type == "" {
		return errdefs.Validationf("recall %s link: link %q missing type", backend, link.ID)
	}
	if link.From.Kind == "" || link.From.ID == "" || link.To.Kind == "" || link.To.ID == "" {
		return errdefs.Validationf("recall %s link: link %q requires from/to refs", backend, link.ID)
	}
	return nil
}

var (
	_ port.ObservationStore = (*observationStore)(nil)
	_ port.LinkStore        = (*linkStore)(nil)
)
