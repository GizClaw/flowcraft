package postgres

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Index is a Postgres-backed retrieval.Index.
//
// One namespace maps to one table retrieval_<ns> with a tsvector column and
// jsonb metadata. Vector scoring is computed client-side in the Pipeline.
type Index struct {
	pool *pgxpool.Pool
}

// Open creates a Postgres-backed Index from a DSN (e.g. "postgres://...").
func Open(ctx context.Context, dsn string) (*Index, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Index{pool: pool}, nil
}

// Close implements retrieval.Index.
func (s *Index) Close() error { s.pool.Close(); return nil }

// Capabilities implements retrieval.Index.
func (s *Index) Capabilities() retrieval.Capabilities {
	return retrieval.Capabilities{
		BM25:   true,
		Vector: false,
		Sparse: false,
		Hybrid: false,

		FilterPushdown: false,
		MaxFilterDepth: -1,
		SupportedOps: []string{
			"eq", "neq", "in", "nin",
			"range", "exists", "missing", "match",
			"contains", "icontains", "contains_any", "contains_all",
			"and", "or", "not",
		},

		Rerank:         false,
		BatchUpsertMax: 1000,
		WriteIsAtomic:  true,

		MaxListPageSize:      1000,
		NativeDeleteByFilter: false,
		SupportedListOrders:  []retrieval.ListOrderBy{retrieval.OrderByTimestampDesc, retrieval.OrderByTimestampAsc, retrieval.OrderByIDAsc},

		ReadAfterWrite: true,
		Distributed:    false,
		Extensions: retrieval.ExtensionCapabilities{
			DocGetter:      true,
			Iterable:       true,
			Count:          true,
			DeleteByFilter: true,
			DropNamespace:  true,
		},
	}
}

func validNS(ns string) bool {
	if ns == "" || len(ns) > 48 {
		return false
	}
	for _, r := range ns {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			return false
		}
	}
	return true
}

func tableName(ns string) string { return "retrieval_" + ns }

func (s *Index) ensureNS(ctx context.Context, ns string) error {
	if !validNS(ns) {
		return errdefs.Validationf("postgres: invalid namespace %q", ns)
	}
	tbl := tableName(ns)
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %q (
			id text PRIMARY KEY,
			content text NOT NULL,
			tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED,
			metadata jsonb,
			vector bytea,
			sparse jsonb,
			ts timestamptz NOT NULL
		)`, tbl),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q USING gin (tsv)`, "ix_"+tbl+"_tsv", tbl),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q USING gin (metadata jsonb_path_ops)`, "ix_"+tbl+"_meta", tbl),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %q (ts DESC)`, "ix_"+tbl+"_ts", tbl),
	}
	for _, q := range stmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return errdefs.NotAvailable(fmt.Errorf("postgres: ensureNS %s: %w", ns, err))
		}
	}
	return nil
}

// Drop implements retrieval.Droppable.
func (s *Index) Drop(ctx context.Context, ns string) error {
	if !validNS(ns) {
		return errdefs.Validationf("postgres: invalid namespace %q", ns)
	}
	_, err := s.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %q`, tableName(ns)))
	return err
}

// Get implements retrieval.DocGetter.
func (s *Index) Get(ctx context.Context, ns, id string) (retrieval.Doc, bool, error) {
	if err := s.ensureNS(ctx, ns); err != nil {
		return retrieval.Doc{}, false, err
	}
	row := s.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT content, metadata::text, vector, sparse::text, ts FROM %q WHERE id=$1`, tableName(ns)), id)
	var (
		content string
		mdText  *string
		vecBlob []byte
		spText  *string
		ts      time.Time
	)
	if err := row.Scan(&content, &mdText, &vecBlob, &spText, &ts); err != nil {
		if err == pgx.ErrNoRows {
			return retrieval.Doc{}, false, nil
		}
		return retrieval.Doc{}, false, err
	}
	d := retrieval.Doc{ID: id, Content: content, Vector: decodeVector(vecBlob), Timestamp: ts.UTC()}
	if mdText != nil && *mdText != "" {
		_ = json.Unmarshal([]byte(*mdText), &d.Metadata)
	}
	if spText != nil && *spText != "" {
		_ = json.Unmarshal([]byte(*spText), &d.SparseVector)
	}
	return d, true, nil
}

// Upsert implements retrieval.Index.
func (s *Index) Upsert(ctx context.Context, ns string, docs []retrieval.Doc) error {
	if err := s.ensureNS(ctx, ns); err != nil {
		return err
	}
	for _, d := range docs {
		if strings.TrimSpace(d.ID) == "" {
			return errdefs.Validationf("postgres: doc id required")
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := fmt.Sprintf(`INSERT INTO %q(id,content,metadata,vector,sparse,ts) VALUES($1,$2,$3::jsonb,$4,$5::jsonb,$6)
		ON CONFLICT(id) DO UPDATE SET content=EXCLUDED.content, metadata=EXCLUDED.metadata, vector=EXCLUDED.vector, sparse=EXCLUDED.sparse, ts=EXCLUDED.ts`, tableName(ns))
	for _, d := range docs {
		md, _ := json.Marshal(d.Metadata)
		sp, _ := json.Marshal(d.SparseVector)
		ts := d.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if _, err := tx.Exec(ctx, q, d.ID, d.Content, string(md), encodeVector(d.Vector), string(sp), ts); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Delete implements retrieval.Index.
func (s *Index) Delete(ctx context.Context, ns string, ids []string) error {
	if err := s.ensureNS(ctx, ns); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %q WHERE id = ANY($1)`, tableName(ns)), ids)
	return err
}

// DeleteByFilter implements retrieval.DeletableByFilter (List+Delete fallback).
func (s *Index) DeleteByFilter(ctx context.Context, ns string, f retrieval.Filter) (int64, error) {
	if isEmptyFilter(f) {
		return 0, retrieval.ErrEmptyDeleteFilter
	}
	var ids []string
	tok := ""
	for {
		page, err := s.List(ctx, ns, retrieval.ListRequest{Filter: f, PageSize: 1000, PageToken: tok})
		if err != nil {
			return 0, err
		}
		for _, d := range page.Items {
			ids = append(ids, d.ID)
		}
		if page.NextPageToken == "" {
			break
		}
		tok = page.NextPageToken
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := s.Delete(ctx, ns, ids); err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
}

// Search implements retrieval.Index.
//
// Native: ts_rank against tsvector for QueryText. QueryVector is used by
// Pipeline (server returns vectors when WithVector is set in Pipeline).
func (s *Index) Search(ctx context.Context, ns string, req retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	if err := s.ensureNS(ctx, ns); err != nil {
		return nil, err
	}
	hasText := strings.TrimSpace(req.QueryText) != ""
	hasVec := len(req.QueryVector) > 0
	hasSparse := len(req.SparseVec) > 0
	if !hasText && !hasVec && !hasSparse {
		return nil, retrieval.ErrNoQuery
	}
	topK := req.TopK
	if topK <= 0 {
		topK = 10
	}
	start := time.Now()

	type cand struct {
		d    retrieval.Doc
		bm25 float64
		cos  float64
	}
	var rows []cand
	if hasText {
		q := tsQuery(req.QueryText)
		query := fmt.Sprintf(
			`SELECT id, content, metadata::text, vector, sparse::text, ts, ts_rank(tsv, plainto_tsquery('simple',$1)) AS score
			 FROM %q WHERE tsv @@ plainto_tsquery('simple',$1)
			 ORDER BY score DESC LIMIT $2 OFFSET $3`, tableName(ns))
		pageSize := topK * 4
		if pageSize < 100 {
			pageSize = 100
		}
		maxScan := topK * 256
		if maxScan < 1000 {
			maxScan = 1000
		}
		for offset := 0; offset < maxScan; offset += pageSize {
			rs, err := s.pool.Query(ctx, query, q, pageSize, offset)
			if err != nil {
				return nil, err
			}
			n := 0
			for rs.Next() {
				n++
				var (
					id, content string
					mdText      *string
					vecBlob     []byte
					spText      *string
					ts          time.Time
					score       float64
				)
				if err := rs.Scan(&id, &content, &mdText, &vecBlob, &spText, &ts, &score); err != nil {
					rs.Close()
					return nil, err
				}
				d := retrieval.Doc{ID: id, Content: content, Vector: decodeVector(vecBlob), Timestamp: ts.UTC()}
				if mdText != nil && *mdText != "" {
					_ = json.Unmarshal([]byte(*mdText), &d.Metadata)
				}
				if spText != nil && *spText != "" {
					_ = json.Unmarshal([]byte(*spText), &d.SparseVector)
				}
				if !retrieval.DocMatchesFilter(d, req.Filter) {
					continue
				}
				rows = append(rows, cand{d: d, bm25: score})
			}
			rs.Close()
			if err := rs.Err(); err != nil {
				return nil, err
			}
			if n < pageSize || (!hasVec && len(rows) >= topK) {
				break
			}
		}
	} else {
		all, err := s.scanAll(ctx, ns, req.Filter, 4*topK)
		if err != nil {
			return nil, err
		}
		for _, d := range all {
			rows = append(rows, cand{d: d})
		}
	}
	if hasVec {
		for i := range rows {
			if len(rows[i].d.Vector) == len(req.QueryVector) {
				rows[i].cos = cosineSim(rows[i].d.Vector, req.QueryVector)
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if hasText && hasVec {
			return rows[i].bm25+rows[i].cos > rows[j].bm25+rows[j].cos
		}
		if hasVec {
			return rows[i].cos > rows[j].cos
		}
		return rows[i].bm25 > rows[j].bm25
	})
	out := make([]retrieval.Hit, 0, topK)
	for _, c := range rows {
		score := c.bm25
		if hasText && hasVec {
			score = c.bm25 + c.cos
		} else if hasVec && !hasText {
			score = c.cos
		}
		if score < req.MinScore {
			continue
		}
		out = append(out, retrieval.Hit{Doc: c.d, Score: score, Scores: map[string]float64{"bm25": c.bm25, "cos": c.cos}})
		if len(out) >= topK {
			break
		}
	}
	return &retrieval.SearchResponse{Hits: out, Took: time.Since(start)}, nil
}

func (s *Index) scanAll(ctx context.Context, ns string, f retrieval.Filter, limit int) ([]retrieval.Doc, error) {
	if limit <= 0 {
		limit = 1000
	}
	rs, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id,content,metadata::text,vector,sparse::text,ts FROM %q ORDER BY ts DESC LIMIT $1`, tableName(ns)), limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []retrieval.Doc
	for rs.Next() {
		var (
			id, content string
			mdText      *string
			vecBlob     []byte
			spText      *string
			ts          time.Time
		)
		if err := rs.Scan(&id, &content, &mdText, &vecBlob, &spText, &ts); err != nil {
			return nil, err
		}
		d := retrieval.Doc{ID: id, Content: content, Vector: decodeVector(vecBlob), Timestamp: ts.UTC()}
		if mdText != nil && *mdText != "" {
			_ = json.Unmarshal([]byte(*mdText), &d.Metadata)
		}
		if spText != nil && *spText != "" {
			_ = json.Unmarshal([]byte(*spText), &d.SparseVector)
		}
		if retrieval.DocMatchesFilter(d, f) {
			out = append(out, d)
		}
	}
	return out, rs.Err()
}

// Count implements retrieval.Countable.
func (s *Index) Count(ctx context.Context, ns string, f retrieval.Filter) (int64, error) {
	if err := s.ensureNS(ctx, ns); err != nil {
		return 0, err
	}
	if where, args, ok := postgresFilterWhere(f, 1); ok {
		var total int64
		query := fmt.Sprintf(`SELECT COUNT(*) FROM %q`, tableName(ns))
		if where != "" {
			query += " WHERE " + where
		}
		if err := s.pool.QueryRow(ctx, query, args...).Scan(&total); err != nil {
			return 0, err
		}
		return total, nil
	}
	rs, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id,content,metadata::text,vector,sparse::text,ts FROM %q`, tableName(ns)))
	if err != nil {
		return 0, err
	}
	defer rs.Close()
	var total int64
	for rs.Next() {
		var (
			id, content string
			mdText      *string
			vecBlob     []byte
			spText      *string
			ts          time.Time
		)
		if err := rs.Scan(&id, &content, &mdText, &vecBlob, &spText, &ts); err != nil {
			return 0, err
		}
		d := retrieval.Doc{ID: id, Content: content, Vector: decodeVector(vecBlob), Timestamp: ts.UTC()}
		if mdText != nil && *mdText != "" {
			_ = json.Unmarshal([]byte(*mdText), &d.Metadata)
		}
		if spText != nil && *spText != "" {
			_ = json.Unmarshal([]byte(*spText), &d.SparseVector)
		}
		if retrieval.DocMatchesFilter(d, f) {
			total++
		}
	}
	return total, rs.Err()
}

func scanPostgresDocs(rs pgx.Rows) ([]retrieval.Doc, error) {
	var out []retrieval.Doc
	for rs.Next() {
		var (
			id, content string
			mdText      *string
			vecBlob     []byte
			spText      *string
			ts          time.Time
		)
		if err := rs.Scan(&id, &content, &mdText, &vecBlob, &spText, &ts); err != nil {
			return nil, err
		}
		d := retrieval.Doc{ID: id, Content: content, Vector: decodeVector(vecBlob), Timestamp: ts.UTC()}
		if mdText != nil && *mdText != "" {
			_ = json.Unmarshal([]byte(*mdText), &d.Metadata)
		}
		if spText != nil && *spText != "" {
			_ = json.Unmarshal([]byte(*spText), &d.SparseVector)
		}
		out = append(out, d)
	}
	return out, rs.Err()
}

func postgresFilterWhere(f retrieval.Filter, startArg int) (string, []any, bool) {
	c := postgresFilterCompiler{next: startArg}
	expr, ok := c.compile(f)
	return expr, c.args, ok
}

type postgresFilterCompiler struct {
	next int
	args []any
}

func (c *postgresFilterCompiler) arg(v any) string {
	c.args = append(c.args, v)
	n := c.next
	c.next++
	return fmt.Sprintf("$%d", n)
}

func (c *postgresFilterCompiler) compile(f retrieval.Filter) (string, bool) {
	if isEmptyFilter(f) {
		return "", true
	}
	if len(f.Match) > 0 || len(f.Contains) > 0 || len(f.IContains) > 0 ||
		len(f.ContainsAny) > 0 || len(f.ContainsAll) > 0 {
		return "", false
	}
	var parts []string

	if f.Not != nil {
		expr, ok := c.compile(*f.Not)
		if !ok {
			return "", false
		}
		parts = append(parts, "NOT ("+postgresTrueExpr(expr)+")")
	}
	for _, sub := range f.And {
		expr, ok := c.compile(sub)
		if !ok {
			return "", false
		}
		if expr != "" {
			parts = append(parts, "("+expr+")")
		}
	}
	if len(f.Or) > 0 {
		orParts := make([]string, 0, len(f.Or))
		for _, sub := range f.Or {
			expr, ok := c.compile(sub)
			if !ok {
				return "", false
			}
			orParts = append(orParts, "("+postgresTrueExpr(expr)+")")
		}
		parts = append(parts, "("+strings.Join(orParts, " OR ")+")")
	}
	for k, v := range f.Eq {
		if !postgresScalar(v) {
			return "", false
		}
		parts = append(parts, fmt.Sprintf(`metadata ->> %s = %s`, c.arg(k), c.arg(postgresTextValue(v))))
	}
	for k, v := range f.Neq {
		if !postgresScalar(v) {
			return "", false
		}
		keyArg := c.arg(k)
		valArg := c.arg(postgresTextValue(v))
		parts = append(parts, fmt.Sprintf(`(NOT (metadata ? %s) OR jsonb_typeof(metadata -> %s) = 'null' OR metadata ->> %s <> %s)`, keyArg, keyArg, keyArg, valArg))
	}
	for k, values := range f.In {
		if len(values) == 0 {
			parts = append(parts, "FALSE")
			continue
		}
		vals, ok := postgresTextValues(values)
		if !ok {
			return "", false
		}
		parts = append(parts, fmt.Sprintf(`metadata ->> %s = ANY(%s)`, c.arg(k), c.arg(vals)))
	}
	for k, values := range f.NotIn {
		if len(values) == 0 {
			continue
		}
		vals, ok := postgresTextValues(values)
		if !ok {
			return "", false
		}
		keyArg := c.arg(k)
		valArg := c.arg(vals)
		parts = append(parts, fmt.Sprintf(`(NOT (metadata ? %s) OR jsonb_typeof(metadata -> %s) = 'null' OR NOT (metadata ->> %s = ANY(%s)))`, keyArg, keyArg, keyArg, valArg))
	}
	for k, r := range f.Range {
		expr, ok := c.rangeExpr(k, r)
		if !ok {
			return "", false
		}
		parts = append(parts, expr)
	}
	for _, k := range f.Exists {
		parts = append(parts, fmt.Sprintf(`metadata ? %s`, c.arg(k)))
	}
	for _, k := range f.Missing {
		parts = append(parts, fmt.Sprintf(`NOT (metadata ? %s)`, c.arg(k)))
	}
	return strings.Join(parts, " AND "), true
}

func (c *postgresFilterCompiler) rangeExpr(k string, r retrieval.Range) (string, bool) {
	keyArg := c.arg(k)
	valueExpr := fmt.Sprintf(`(metadata ->> %s)::double precision`, keyArg)
	parts := []string{
		fmt.Sprintf(`metadata ? %s`, keyArg),
		fmt.Sprintf(`jsonb_typeof(metadata -> %s) = 'number'`, keyArg),
	}
	for _, bound := range []struct {
		op string
		v  any
	}{
		{">", r.Gt},
		{">=", r.Gte},
		{"<", r.Lt},
		{"<=", r.Lte},
	} {
		if bound.v == nil {
			continue
		}
		fv, ok := sqlFloat64(bound.v)
		if !ok {
			return "", false
		}
		parts = append(parts, fmt.Sprintf(`%s %s %s`, valueExpr, bound.op, c.arg(fv)))
	}
	return "(" + strings.Join(parts, " AND ") + ")", true
}

func postgresTrueExpr(expr string) string {
	if expr == "" {
		return "TRUE"
	}
	return expr
}

func postgresScalar(v any) bool {
	if v == nil {
		return false
	}
	switch v.(type) {
	case string, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	default:
		return false
	}
}

func postgresTextValue(v any) string {
	return fmt.Sprint(v)
}

func postgresTextValues(values []any) ([]string, bool) {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !postgresScalar(v) {
			return nil, false
		}
		out = append(out, postgresTextValue(v))
	}
	return out, true
}

func sqlFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	default:
		return 0, false
	}
}

// List implements retrieval.Index.
func (s *Index) List(ctx context.Context, ns string, req retrieval.ListRequest) (*retrieval.ListResponse, error) {
	if err := s.ensureNS(ctx, ns); err != nil {
		return nil, err
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}
	if pageSize > 1000 {
		pageSize = 1000
	}
	offset, err := retrieval.DecodeListPageTokenFor(req.PageToken, req)
	if err != nil {
		return nil, err
	}
	order := "ts DESC, id ASC"
	switch req.OrderBy {
	case retrieval.OrderByTimestampAsc:
		order = "ts ASC, id ASC"
	case retrieval.OrderByIDAsc:
		order = "id ASC"
	}
	if where, args, ok := postgresFilterWhere(req.Filter, 1); ok {
		var total int64
		countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM %q`, tableName(ns))
		if where != "" {
			countQuery += " WHERE " + where
		}
		if err := s.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
			return nil, err
		}
		limitArg := len(args) + 1
		offsetArg := len(args) + 2
		query := fmt.Sprintf(`SELECT id,content,metadata::text,vector,sparse::text,ts FROM %q`, tableName(ns))
		if where != "" {
			query += " WHERE " + where
		}
		query += fmt.Sprintf(` ORDER BY %s LIMIT $%d OFFSET $%d`, order, limitArg, offsetArg)
		selectArgs := append([]any(nil), args...)
		selectArgs = append(selectArgs, pageSize, offset)
		rs, err := s.pool.Query(ctx,
			query,
			selectArgs...)
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		page, err := scanPostgresDocs(rs)
		if err != nil {
			return nil, err
		}
		for i := range page {
			page[i] = projectDoc(page[i], req.Project, req.WithVector)
		}
		next := ""
		if int64(offset+len(page)) < total {
			next, err = retrieval.EncodeListPageTokenFor(offset+len(page), req)
			if err != nil {
				return nil, err
			}
		}
		return &retrieval.ListResponse{Items: page, NextPageToken: next, Total: total}, nil
	}
	rs, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id,content,metadata::text,vector,sparse::text,ts FROM %q ORDER BY %s`, tableName(ns), order))
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var all []retrieval.Doc
	for rs.Next() {
		var (
			id, content string
			mdText      *string
			vecBlob     []byte
			spText      *string
			ts          time.Time
		)
		if err := rs.Scan(&id, &content, &mdText, &vecBlob, &spText, &ts); err != nil {
			return nil, err
		}
		d := retrieval.Doc{ID: id, Content: content, Vector: decodeVector(vecBlob), Timestamp: ts.UTC()}
		if mdText != nil && *mdText != "" {
			_ = json.Unmarshal([]byte(*mdText), &d.Metadata)
		}
		if spText != nil && *spText != "" {
			_ = json.Unmarshal([]byte(*spText), &d.SparseVector)
		}
		if retrieval.DocMatchesFilter(d, req.Filter) {
			all = append(all, d)
		}
	}
	total := int64(len(all))
	if offset >= len(all) {
		return &retrieval.ListResponse{Items: []retrieval.Doc{}, Total: total}, nil
	}
	end := offset + pageSize
	if end > len(all) {
		end = len(all)
	}
	page := all[offset:end]
	for i := range page {
		page[i] = projectDoc(page[i], req.Project, req.WithVector)
	}
	next := ""
	if end < len(all) {
		next, err = retrieval.EncodeListPageTokenFor(end, req)
		if err != nil {
			return nil, err
		}
	}
	return &retrieval.ListResponse{Items: page, NextPageToken: next, Total: total}, nil
}

// Iterate implements retrieval.Iterable.
func (s *Index) Iterate(ctx context.Context, ns string, cursor string, batch int) ([]retrieval.Doc, string, error) {
	if err := s.ensureNS(ctx, ns); err != nil {
		return nil, "", err
	}
	if batch <= 0 {
		batch = 100
	}
	rs, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id,content,metadata::text,vector,sparse::text,ts FROM %q WHERE id > $1 ORDER BY id ASC LIMIT $2`, tableName(ns)), cursor, batch)
	if err != nil {
		return nil, "", err
	}
	defer rs.Close()
	var out []retrieval.Doc
	for rs.Next() {
		var (
			id, content string
			mdText      *string
			vecBlob     []byte
			spText      *string
			ts          time.Time
		)
		if err := rs.Scan(&id, &content, &mdText, &vecBlob, &spText, &ts); err != nil {
			return nil, "", err
		}
		d := retrieval.Doc{ID: id, Content: content, Vector: decodeVector(vecBlob), Timestamp: ts.UTC()}
		if mdText != nil && *mdText != "" {
			_ = json.Unmarshal([]byte(*mdText), &d.Metadata)
		}
		if spText != nil && *spText != "" {
			_ = json.Unmarshal([]byte(*spText), &d.SparseVector)
		}
		out = append(out, d)
	}
	next := ""
	if len(out) > 0 {
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

func tsQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}
	return q
}

func encodeVector(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(f))
	}
	return buf
}

func decodeVector(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

func cosineSim(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func isEmptyFilter(f retrieval.Filter) bool {
	return len(f.And) == 0 && len(f.Or) == 0 && f.Not == nil &&
		len(f.Eq) == 0 && len(f.Neq) == 0 && len(f.In) == 0 && len(f.NotIn) == 0 &&
		len(f.Range) == 0 && len(f.Exists) == 0 && len(f.Missing) == 0 && len(f.Match) == 0 &&
		len(f.Contains) == 0 && len(f.IContains) == 0 && len(f.ContainsAny) == 0 && len(f.ContainsAll) == 0
}

func projectDoc(d retrieval.Doc, project []string, withVector bool) retrieval.Doc {
	if !withVector {
		d.Vector = nil
		d.SparseVector = nil
	}
	if len(project) == 0 || d.Metadata == nil {
		return d
	}
	md := make(map[string]any, len(project))
	for _, k := range project {
		if v, ok := d.Metadata[k]; ok {
			md[k] = v
		}
	}
	d.Metadata = md
	return d
}
