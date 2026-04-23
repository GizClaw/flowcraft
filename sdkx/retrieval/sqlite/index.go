package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/retrieval"

	_ "modernc.org/sqlite"
)

// Index is a SQLite-backed retrieval.Index.
//
// Hybrid search is performed by the Pipeline (Capabilities.Hybrid=false);
// only BM25 over content (FTS5) is native. Vector / sparse are client-side.
type Index struct {
	db *sql.DB

	mu       sync.Mutex
	prepared map[string]struct{}
}

// Open opens or creates a SQLite database at path with the recommended pragmas.
//
// Use ":memory:" for an in-process database.
func Open(path string) (*Index, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA temp_store=MEMORY",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite: pragma %q: %w", p, err)
		}
	}
	return &Index{db: db, prepared: map[string]struct{}{}}, nil
}

// Close implements retrieval.Index.
func (s *Index) Close() error { return s.db.Close() }

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
	}
}

func (s *Index) ensureNS(ctx context.Context, ns string) error {
	if !validNS(ns) {
		return errdefs.Validationf("sqlite: invalid namespace %q", ns)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.prepared[ns]; ok {
		return nil
	}
	tbl := "docs_" + ns
	fts := "fts_" + ns
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS "%s" (
			id TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			metadata BLOB,
			vector BLOB,
			sparse BLOB,
			ts INTEGER NOT NULL
		)`, tbl),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS "ix_%s_ts" ON "%s"(ts DESC)`, tbl, tbl),
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS "%s" USING fts5(content, content='%s', content_rowid='rowid', tokenize='unicode61')`, fts, tbl),
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("sqlite: ensureNS %s: %w", ns, err)
		}
	}
	s.prepared[ns] = struct{}{}
	return nil
}

// Drop implements retrieval.Droppable.
func (s *Index) Drop(ctx context.Context, ns string) error {
	if !validNS(ns) {
		return errdefs.Validationf("sqlite: invalid namespace %q", ns)
	}
	for _, q := range []string{
		fmt.Sprintf(`DROP TABLE IF EXISTS "fts_%s"`, ns),
		fmt.Sprintf(`DROP TABLE IF EXISTS "docs_%s"`, ns),
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	s.mu.Lock()
	delete(s.prepared, ns)
	s.mu.Unlock()
	return nil
}

// Get implements retrieval.DocGetter.
func (s *Index) Get(ctx context.Context, ns, id string) (retrieval.Doc, bool, error) {
	if err := s.ensureNS(ctx, ns); err != nil {
		return retrieval.Doc{}, false, err
	}
	var (
		content    string
		mdRaw      []byte
		vecBlob    []byte
		sparseBlob []byte
		tsUnix     int64
	)
	row := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT content, metadata, vector, sparse, ts FROM "docs_%s" WHERE id=?`, ns), id)
	if err := row.Scan(&content, &mdRaw, &vecBlob, &sparseBlob, &tsUnix); err != nil {
		if err == sql.ErrNoRows {
			return retrieval.Doc{}, false, nil
		}
		return retrieval.Doc{}, false, err
	}
	d := retrieval.Doc{
		ID:           id,
		Content:      content,
		Vector:       decodeVector(vecBlob),
		SparseVector: decodeSparse(sparseBlob),
		Timestamp:    time.Unix(0, tsUnix).UTC(),
	}
	if len(mdRaw) > 0 {
		_ = json.Unmarshal(mdRaw, &d.Metadata)
	}
	return d, true, nil
}

// Upsert implements retrieval.Index.
func (s *Index) Upsert(ctx context.Context, ns string, docs []retrieval.Doc) error {
	if err := s.ensureNS(ctx, ns); err != nil {
		return err
	}
	var partial []retrieval.DocUpsertResult
	for _, d := range docs {
		if strings.TrimSpace(d.ID) == "" {
			partial = append(partial, retrieval.DocUpsertResult{ID: d.ID, Err: errdefs.Validationf("sqlite: doc id required")})
		}
	}
	if len(partial) > 0 {
		return &retrieval.PartialError{Results: partial}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	upsertSQL := fmt.Sprintf(`INSERT INTO "docs_%s"(id,content,metadata,vector,sparse,ts) VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET content=excluded.content,metadata=excluded.metadata,vector=excluded.vector,sparse=excluded.sparse,ts=excluded.ts`, ns)
	stmt, err := tx.PrepareContext(ctx, upsertSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	ftsDel, err := tx.PrepareContext(ctx, fmt.Sprintf(`DELETE FROM "fts_%s" WHERE rowid=(SELECT rowid FROM "docs_%s" WHERE id=?)`, ns, ns))
	if err != nil {
		return err
	}
	defer ftsDel.Close()
	ftsIns, err := tx.PrepareContext(ctx, fmt.Sprintf(`INSERT INTO "fts_%s"(rowid,content) VALUES((SELECT rowid FROM "docs_%s" WHERE id=?),?)`, ns, ns))
	if err != nil {
		return err
	}
	defer ftsIns.Close()

	for _, d := range docs {
		mdBytes, _ := json.Marshal(d.Metadata)
		ts := d.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		if _, err := ftsDel.ExecContext(ctx, d.ID); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, d.ID, d.Content, mdBytes, encodeVector(d.Vector), encodeSparse(d.SparseVector), ts.UnixNano()); err != nil {
			return err
		}
		if _, err := ftsIns.ExecContext(ctx, d.ID, d.Content); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Delete implements retrieval.Index.
func (s *Index) Delete(ctx context.Context, ns string, ids []string) error {
	if err := s.ensureNS(ctx, ns); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM "fts_%s" WHERE rowid=(SELECT rowid FROM "docs_%s" WHERE id=?)`, ns, ns), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM "docs_%s" WHERE id=?`, ns), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteByFilter implements retrieval.DeletableByFilter.
//
// Implementation uses List+Delete (no native bulk DELETE … WHERE JSON()).
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
// Native scoring: BM25 over FTS5 when QueryText is present.
// QueryVector / SparseVec are returned untouched in the Hit list ordered by ts (caller
// performs vector scoring inside the Pipeline).
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
		q := ftsQuery(req.QueryText)
		// negative bm25 (lower is better in fts5) → flip sign for descending
		query := fmt.Sprintf(`SELECT d.id, d.content, d.metadata, d.vector, d.sparse, d.ts, -bm25("fts_%s") AS score
			FROM "fts_%s" JOIN "docs_%s" d ON d.rowid = "fts_%s".rowid
			WHERE "fts_%s" MATCH ?
			ORDER BY score DESC
			LIMIT ?`, ns, ns, ns, ns, ns)
		r, err := s.db.QueryContext(ctx, query, q, topK*4)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		for r.Next() {
			var (
				id, content                string
				mdRaw, vecBlob, sparseBlob []byte
				tsUnix                     int64
				bm                         float64
			)
			if err := r.Scan(&id, &content, &mdRaw, &vecBlob, &sparseBlob, &tsUnix, &bm); err != nil {
				return nil, err
			}
			d := retrieval.Doc{
				ID: id, Content: content,
				Vector: decodeVector(vecBlob), SparseVector: decodeSparse(sparseBlob),
				Timestamp: time.Unix(0, tsUnix).UTC(),
			}
			if len(mdRaw) > 0 {
				_ = json.Unmarshal(mdRaw, &d.Metadata)
			}
			if !retrieval.DocMatchesFilter(d, req.Filter) {
				continue
			}
			rows = append(rows, cand{d: d, bm25: bm})
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
			si := rows[i].bm25 + rows[i].cos
			sj := rows[j].bm25 + rows[j].cos
			return si > sj
		}
		if hasVec {
			return rows[i].cos > rows[j].cos
		}
		return rows[i].bm25 > rows[j].bm25
	})

	out := make([]retrieval.Hit, 0, topK)
	for _, c := range rows {
		score := c.bm25
		if hasVec && !hasText {
			score = c.cos
		}
		if score < req.MinScore {
			continue
		}
		out = append(out, retrieval.Hit{
			Doc:    c.d,
			Score:  score,
			Scores: map[string]float64{"bm25": c.bm25, "cos": c.cos},
		})
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
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id,content,metadata,vector,sparse,ts FROM "docs_%s" ORDER BY ts DESC LIMIT ?`, ns), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []retrieval.Doc
	for rows.Next() {
		var (
			id, content                string
			mdRaw, vecBlob, sparseBlob []byte
			tsUnix                     int64
		)
		if err := rows.Scan(&id, &content, &mdRaw, &vecBlob, &sparseBlob, &tsUnix); err != nil {
			return nil, err
		}
		d := retrieval.Doc{
			ID: id, Content: content,
			Vector: decodeVector(vecBlob), SparseVector: decodeSparse(sparseBlob),
			Timestamp: time.Unix(0, tsUnix).UTC(),
		}
		if len(mdRaw) > 0 {
			_ = json.Unmarshal(mdRaw, &d.Metadata)
		}
		if retrieval.DocMatchesFilter(d, f) {
			out = append(out, d)
		}
	}
	return out, nil
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
	offset, err := retrieval.DecodeListPageToken(req.PageToken)
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
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id,content,metadata,vector,sparse,ts FROM "docs_%s" ORDER BY %s`, ns, order))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []retrieval.Doc
	for rows.Next() {
		var (
			id, content                string
			mdRaw, vecBlob, sparseBlob []byte
			tsUnix                     int64
		)
		if err := rows.Scan(&id, &content, &mdRaw, &vecBlob, &sparseBlob, &tsUnix); err != nil {
			return nil, err
		}
		d := retrieval.Doc{
			ID: id, Content: content,
			Vector: decodeVector(vecBlob), SparseVector: decodeSparse(sparseBlob),
			Timestamp: time.Unix(0, tsUnix).UTC(),
		}
		if len(mdRaw) > 0 {
			_ = json.Unmarshal(mdRaw, &d.Metadata)
		}
		if retrieval.DocMatchesFilter(d, req.Filter) {
			all = append(all, d)
		}
	}
	total := int64(len(all))
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
		next, err = retrieval.EncodeListPageToken(end)
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
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id,content,metadata,vector,sparse,ts FROM "docs_%s" WHERE id>? ORDER BY id ASC LIMIT ?`, ns), cursor, batch)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []retrieval.Doc
	for rows.Next() {
		var (
			id, content                string
			mdRaw, vecBlob, sparseBlob []byte
			tsUnix                     int64
		)
		if err := rows.Scan(&id, &content, &mdRaw, &vecBlob, &sparseBlob, &tsUnix); err != nil {
			return nil, "", err
		}
		d := retrieval.Doc{
			ID: id, Content: content,
			Vector: decodeVector(vecBlob), SparseVector: decodeSparse(sparseBlob),
			Timestamp: time.Unix(0, tsUnix).UTC(),
		}
		if len(mdRaw) > 0 {
			_ = json.Unmarshal(mdRaw, &d.Metadata)
		}
		out = append(out, d)
	}
	next := ""
	if len(out) > 0 {
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

func validNS(ns string) bool {
	if ns == "" || len(ns) > 64 {
		return false
	}
	for _, r := range ns {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			return false
		}
	}
	return true
}

func ftsQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}
	var b strings.Builder
	for _, w := range strings.Fields(q) {
		clean := strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				return r
			}
			return -1
		}, w)
		if clean == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(" OR ")
		}
		b.WriteString(clean)
		b.WriteByte('*')
	}
	if b.Len() == 0 {
		return q
	}
	return b.String()
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

func encodeSparse(m map[string]float32) []byte {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m)
	return b
}

func decodeSparse(b []byte) map[string]float32 {
	if len(b) == 0 {
		return nil
	}
	var m map[string]float32
	_ = json.Unmarshal(b, &m)
	return m
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
