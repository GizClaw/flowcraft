package postgres

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/jackc/pgx/v5"
)

type observationStore struct {
	b *Backend
}

func (s *observationStore) Append(ctx context.Context, observations []domain.Observation) error {
	if len(observations) == 0 {
		return nil
	}
	tx, err := s.b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, obs := range observations {
		if obs.ID == "" {
			return errdefs.Validationf("recall postgres observation: observation id is required")
		}
		if obs.Scope.RuntimeID == "" {
			return errdefs.Validationf("recall postgres observation: observation %q missing scope.runtime_id", obs.ID)
		}
		runtimeID, userID := sqlstmt.ScopeParts(obs.Scope)
		payload, err := sqlstmt.EncodeJSON(obs)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO recall_observations(runtime_id, user_id, id, kind, source_id, observed_at_ns, payload_json)
			VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT DO NOTHING
		`, runtimeID, userID, obs.ID, string(obs.Kind), obs.SourceID, obs.ObservedAt.UnixNano(), payload)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			continue
		}
		var existingPayload string
		err = tx.QueryRow(ctx, `
			SELECT payload_json FROM recall_observations
			WHERE runtime_id = $1 AND user_id = $2 AND id = $3
			FOR UPDATE
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
			return errdefs.Conflictf("recall postgres observation: duplicate observation id %q in scope", obs.ID)
		}
		if !changed {
			continue
		}
		payload, err = sqlstmt.EncodeJSON(merged)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE recall_observations
			SET kind = $1, source_id = $2, observed_at_ns = $3, payload_json = $4
			WHERE runtime_id = $5 AND user_id = $6 AND id = $7
		`, string(merged.Kind), merged.SourceID, merged.ObservedAt.UnixNano(), payload, runtimeID, userID, obs.ID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *observationStore) Get(ctx context.Context, scope domain.Scope, observationID string) (domain.Observation, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	var payload string
	err := s.b.pool.QueryRow(ctx, `
		SELECT payload_json FROM recall_observations
		WHERE runtime_id = $1 AND user_id = $2 AND id = $3
	`, runtimeID, userID, observationID).Scan(&payload)
	if err != nil {
		if err == pgx.ErrNoRows {
			return domain.Observation{}, port.ErrNotFound
		}
		return domain.Observation{}, err
	}
	return sqlstmt.DecodeJSON[domain.Observation](payload)
}

func (s *observationStore) List(ctx context.Context, scope domain.Scope, query port.ObservationListQuery) ([]domain.Observation, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	sqlText, args := postgresObservationListSQL(runtimeID, userID, query)
	rows, err := s.b.pool.Query(ctx, sqlText, args...)
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
	_, err := s.b.pool.Exec(ctx, fmt.Sprintf(`
		DELETE FROM recall_observations
		WHERE runtime_id = $1 AND user_id = $2 AND id IN (%s)
	`, phs(3, len(ids))), args...)
	return err
}

func (s *observationStore) DeleteByScope(ctx context.Context, scope domain.Scope) (int, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	tag, err := s.b.pool.Exec(ctx, `
		DELETE FROM recall_observations WHERE runtime_id = $1 AND user_id = $2
	`, runtimeID, userID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *observationStore) Close() error { return s.b.Close() }

type linkStore struct {
	b *Backend
}

func (s *linkStore) Append(ctx context.Context, links []domain.FactLink) error {
	if len(links) == 0 {
		return nil
	}
	tx, err := s.b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, link := range links {
		if err := validateLink(link, "postgres"); err != nil {
			return err
		}
		payload, err := sqlstmt.EncodeJSON(link)
		if err != nil {
			return err
		}
		runtimeID, userID := sqlstmt.ScopeParts(link.Scope)
		tag, err := tx.Exec(ctx, `
			INSERT INTO recall_links(runtime_id, user_id, id, type, from_kind, from_id, to_kind, to_id, merge_key, created_at_ns, payload_json)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT DO NOTHING
		`, runtimeID, userID, link.ID, string(link.Type), string(link.From.Kind), link.From.ID, string(link.To.Kind), link.To.ID, link.MergeKey, link.CreatedAt.UnixNano(), payload)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			continue
		}
		if link.MergeKey != "" {
			existingByMergeKey, err := s.findByMergeKeyTx(ctx, tx, runtimeID, userID, link.MergeKey)
			if err != nil {
				return err
			}
			if existingByMergeKey != nil {
				continue
			}
		}
		existing, err := s.getByIDTx(ctx, tx, runtimeID, userID, link.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(existing.Clone(), link.Clone()) {
			return errdefs.Conflictf("recall postgres link: duplicate link id %q in scope", link.ID)
		}
	}
	return tx.Commit(ctx)
}

func (s *linkStore) getByIDTx(ctx context.Context, tx pgx.Tx, runtimeID, userID, linkID string) (domain.FactLink, error) {
	var payload string
	err := tx.QueryRow(ctx, `
		SELECT payload_json FROM recall_links
		WHERE runtime_id = $1 AND user_id = $2 AND id = $3
	`, runtimeID, userID, linkID).Scan(&payload)
	if err != nil {
		if err == pgx.ErrNoRows {
			return domain.FactLink{}, errdefs.Conflictf("recall postgres link: conflict did not resolve to existing link %q", linkID)
		}
		return domain.FactLink{}, err
	}
	return sqlstmt.DecodeJSON[domain.FactLink](payload)
}

func (s *linkStore) findByMergeKeyTx(ctx context.Context, tx pgx.Tx, runtimeID, userID, mergeKey string) (*domain.FactLink, error) {
	var payload string
	err := tx.QueryRow(ctx, `
		SELECT payload_json FROM recall_links
		WHERE runtime_id = $1 AND user_id = $2 AND merge_key = $3
	`, runtimeID, userID, mergeKey).Scan(&payload)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	link, err := sqlstmt.DecodeJSON[domain.FactLink](payload)
	if err != nil {
		return nil, err
	}
	return &link, nil
}

func (s *linkStore) Get(ctx context.Context, scope domain.Scope, linkID string) (domain.FactLink, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	var payload string
	err := s.b.pool.QueryRow(ctx, `
		SELECT payload_json FROM recall_links
		WHERE runtime_id = $1 AND user_id = $2 AND id = $3
	`, runtimeID, userID, linkID).Scan(&payload)
	if err != nil {
		if err == pgx.ErrNoRows {
			return domain.FactLink{}, port.ErrNotFound
		}
		return domain.FactLink{}, err
	}
	return sqlstmt.DecodeJSON[domain.FactLink](payload)
}

func (s *linkStore) List(ctx context.Context, scope domain.Scope, query port.LinkListQuery) ([]domain.FactLink, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	rows, err := s.b.pool.Query(ctx, `
		SELECT payload_json FROM recall_links
		WHERE runtime_id = $1 AND user_id = $2
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
	rows, err := s.b.pool.Query(ctx, `
		SELECT payload_json FROM recall_links
		WHERE runtime_id = $1 AND user_id = $2
		  AND ((from_kind = $3 AND from_id = $4) OR (to_kind = $3 AND to_id = $4))
		ORDER BY created_at_ns ASC, id ASC
	`, runtimeID, userID, string(node.Kind), node.ID)
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
	err := s.b.pool.QueryRow(ctx, `
		SELECT payload_json FROM recall_links
		WHERE runtime_id = $1 AND user_id = $2 AND merge_key = $3
	`, runtimeID, userID, mergeKey).Scan(&payload)
	if err != nil {
		if err == pgx.ErrNoRows {
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
	_, err := s.b.pool.Exec(ctx, fmt.Sprintf(`
		DELETE FROM recall_links
		WHERE runtime_id = $1 AND user_id = $2 AND id IN (%s)
	`, phs(3, len(ids))), args...)
	return err
}

func (s *linkStore) DeleteByNode(ctx context.Context, scope domain.Scope, node domain.GraphNodeRef) (int, error) {
	if node.Kind == "" || node.ID == "" {
		return 0, nil
	}
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	tag, err := s.b.pool.Exec(ctx, `
		DELETE FROM recall_links
		WHERE runtime_id = $1 AND user_id = $2
		  AND ((from_kind = $3 AND from_id = $4) OR (to_kind = $3 AND to_id = $4))
	`, runtimeID, userID, string(node.Kind), node.ID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *linkStore) DeleteByScope(ctx context.Context, scope domain.Scope) (int, error) {
	runtimeID, userID := sqlstmt.ScopeParts(scope)
	tag, err := s.b.pool.Exec(ctx, `
		DELETE FROM recall_links WHERE runtime_id = $1 AND user_id = $2
	`, runtimeID, userID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *linkStore) Close() error { return s.b.Close() }

type payloadRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanObservations(rows payloadRows) ([]domain.Observation, error) {
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

func scanLinks(rows payloadRows) ([]domain.FactLink, error) {
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

func postgresObservationListSQL(runtimeID, userID string, query port.ObservationListQuery) (string, []any) {
	args := []any{runtimeID, userID}
	sqlText := `
		SELECT payload_json FROM recall_observations
		WHERE runtime_id = $1 AND user_id = $2
	`
	if len(query.Kinds) > 0 {
		sqlText += fmt.Sprintf(" AND kind IN (%s)", phs(len(args)+1, len(query.Kinds)))
		for _, kind := range query.Kinds {
			args = append(args, string(kind))
		}
	}
	if query.SourceID != "" {
		args = append(args, query.SourceID)
		sqlText += fmt.Sprintf(" AND source_id = %s", ph(len(args)))
	}
	sqlText += " ORDER BY observed_at_ns ASC, id ASC"
	if query.Limit > 0 {
		args = append(args, query.Limit)
		sqlText += fmt.Sprintf(" LIMIT %s", ph(len(args)))
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
