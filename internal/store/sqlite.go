// Package store provides a SQLite-backed implementation of model.Store.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/internal/errcode"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/store/migrations"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/pressly/goose/v3"
	"github.com/rs/xid"

	otellog "go.opentelemetry.io/otel/log"
	_ "modernc.org/sqlite"
)

// SQLiteStore implements model.Store using SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (or creates) a SQLite database and runs migrations.
func NewSQLiteStore(ctx context.Context, dsn string) (*SQLiteStore, error) {
	if dir := filepath.Dir(dsn); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: create dir %q: %w", dir, err)
		}
	}
	connStr := fmt.Sprintf("file:%s?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)", dsn)
	db, err := sql.Open("sqlite", connStr)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite %q: %w", dsn, err)
	}

	// Configure connection pool for SQLite (single-writer model)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping sqlite: %w", err)
	}
	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}

	if err := os.Chmod(dsn, 0o600); err != nil && !os.IsNotExist(err) {
		telemetry.Warn(ctx, "store: chmod 0600 failed", otellog.String("dsn", dsn), otellog.String("error", err.Error()))
	}

	telemetry.Info(ctx, "SQLite store opened", otellog.String("dsn", dsn))
	return s, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) migrate() error {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	goose.SetLogger(goose.NopLogger())
	return goose.Up(s.db, ".")
}

// withTx runs fn inside a transaction.
func (s *SQLiteStore) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			telemetry.Warn(ctx, "store: rollback failed",
				otellog.String("rollback_error", rbErr.Error()),
				otellog.String("original_error", err.Error()))
		}
		return err
	}
	return tx.Commit()
}

// --- helpers ---

func timeStr(t time.Time) string {
	if t.IsZero() {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func timeStrPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func marshalJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// encodeCursor encodes (created_at, id) into a base64 opaque cursor.
func encodeCursor(createdAt, id string) string {
	return base64.StdEncoding.EncodeToString([]byte(createdAt + "|" + id))
}

// decodeCursor decodes a base64 cursor back into (created_at, id).
func decodeCursor(cursor string) (createdAt, id string, err error) {
	b, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", fmt.Errorf("store: invalid cursor: %w", err)
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("store: malformed cursor")
	}
	return parts[0], parts[1], nil
}

// --- Agent operations ---

func (s *SQLiteStore) ListAgents(ctx context.Context, opts model.ListOptions) ([]*model.Agent, *model.ListResult, error) {
	limit := opts.EffectiveLimit()

	query := `SELECT id, name, type, description, config, strategy, input_schema, output_schema, created_at, updated_at FROM agents`
	var args []any
	if opts.Cursor != "" {
		cursorTime, cursorID, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, nil, err
		}
		query += ` WHERE (created_at, id) < (?, ?)`
		args = append(args, cursorTime, cursorID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list agents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var apps []*model.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, nil, err
		}
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	lr := &model.ListResult{}
	if len(apps) > limit {
		apps = apps[:limit]
		lr.HasMore = true
		last := apps[len(apps)-1]
		lr.NextCursor = encodeCursor(timeStr(last.CreatedAt), last.AgentID)
	}
	return apps, lr, nil
}

func (s *SQLiteStore) CreateAgent(ctx context.Context, a *model.Agent) (*model.Agent, error) {
	if a.AgentID == "" {
		a.AgentID = xid.New().String()
	}
	now := time.Now()
	a.CreatedAt = now
	a.UpdatedAt = now

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agents (id, name, type, description, config, strategy, input_schema, output_schema, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.AgentID, a.Name, string(a.Type), a.Description,
		marshalJSON(a.Config), marshalJSON(a.StrategyDef),
		marshalJSON(a.InputSchema), marshalJSON(a.OutputSchema),
		timeStr(a.CreatedAt), timeStr(a.UpdatedAt))
	if err != nil {
		return nil, fmt.Errorf("store: create agent: %w", err)
	}
	return a, nil
}

func (s *SQLiteStore) GetAgent(ctx context.Context, id string) (*model.Agent, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, type, description, config, strategy, input_schema, output_schema, created_at, updated_at
		 FROM agents WHERE id = ?`, id)
	a, err := scanAgentRow(row)
	if err == sql.ErrNoRows {
		return nil, errdefs.NotFoundf("agent %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get agent %q: %w", id, err)
	}
	return a, nil
}

func (s *SQLiteStore) UpdateAgent(ctx context.Context, a *model.Agent) (*model.Agent, error) {
	a.UpdatedAt = time.Now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET name=?, type=?, description=?, config=?, strategy=?, input_schema=?, output_schema=?, updated_at=?
		 WHERE id=?`,
		a.Name, string(a.Type), a.Description,
		marshalJSON(a.Config), marshalJSON(a.StrategyDef),
		marshalJSON(a.InputSchema), marshalJSON(a.OutputSchema),
		timeStr(a.UpdatedAt), a.AgentID)
	if err != nil {
		return nil, fmt.Errorf("store: update agent: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, errdefs.NotFoundf("agent %s not found", a.AgentID)
	}
	return a, nil
}

func (s *SQLiteStore) DeleteAgent(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		// Check CoPilot constraint.
		var agentType string
		err := tx.QueryRowContext(ctx, `SELECT type FROM agents WHERE id = ?`, id).Scan(&agentType)
		if err == sql.ErrNoRows {
			return errdefs.NotFoundf("agent %s not found", id)
		}
		if err != nil {
			return fmt.Errorf("store: check agent type: %w", err)
		}
		if model.AgentType(agentType) == model.AgentTypeCoPilot {
			return errcode.MethodNotAllowedf("copilot agent cannot be deleted")
		}

		// Cascade delete in dependency order.
		cascadeDeletes := []string{
			`DELETE FROM execution_events WHERE run_id IN (SELECT id FROM workflow_runs WHERE agent_id = ?)`,
			`DELETE FROM workflow_runs WHERE agent_id = ?`,
			`DELETE FROM messages WHERE conversation_id IN (SELECT id FROM conversations WHERE agent_id = ?)`,
			`DELETE FROM conversations WHERE agent_id = ?`,
			`DELETE FROM graph_versions WHERE agent_id = ?`,
			`DELETE FROM dataset_documents WHERE dataset_id IN (SELECT id FROM datasets WHERE agent_id = ?)`,
			`DELETE FROM datasets WHERE agent_id = ?`,
			`DELETE FROM agents WHERE id = ?`,
		}
		for _, q := range cascadeDeletes {
			if _, err = tx.ExecContext(ctx, q, id); err != nil {
				return fmt.Errorf("store: cascade delete agent: %w", err)
			}
		}
		return nil
	})
}

func scanAgent(rows *sql.Rows) (*model.Agent, error) {
	var (
		a                                 model.Agent
		agentType                         string
		configJSON, strategyJSON          sql.NullString
		inputSchemaJSON, outputSchemaJSON sql.NullString
		createdAtStr, updatedAtStr        string
	)
	if err := rows.Scan(&a.AgentID, &a.Name, &agentType, &a.Description,
		&configJSON, &strategyJSON, &inputSchemaJSON, &outputSchemaJSON,
		&createdAtStr, &updatedAtStr); err != nil {
		return nil, fmt.Errorf("store: scan agent: %w", err)
	}
	a.Type = model.AgentType(agentType)
	a.CreatedAt = parseTime(createdAtStr)
	a.UpdatedAt = parseTime(updatedAtStr)
	unmarshalAgentJSON(&a, configJSON.String, strategyJSON.String, inputSchemaJSON.String, outputSchemaJSON.String)
	return &a, nil
}

func scanAgentRow(row *sql.Row) (*model.Agent, error) {
	var (
		a                                 model.Agent
		agentType                         string
		configJSON, strategyJSON          sql.NullString
		inputSchemaJSON, outputSchemaJSON sql.NullString
		createdAtStr, updatedAtStr        string
	)
	if err := row.Scan(&a.AgentID, &a.Name, &agentType, &a.Description,
		&configJSON, &strategyJSON, &inputSchemaJSON, &outputSchemaJSON,
		&createdAtStr, &updatedAtStr); err != nil {
		return nil, err
	}
	a.Type = model.AgentType(agentType)
	a.CreatedAt = parseTime(createdAtStr)
	a.UpdatedAt = parseTime(updatedAtStr)
	unmarshalAgentJSON(&a, configJSON.String, strategyJSON.String, inputSchemaJSON.String, outputSchemaJSON.String)
	return &a, nil
}

func unmarshalAgentJSON(a *model.Agent, configJSON, strategyJSON, inputSchemaJSON, outputSchemaJSON string) {
	if configJSON != "" {
		_ = json.Unmarshal([]byte(configJSON), &a.Config)
	}
	if strategyJSON != "" && strategyJSON != "null" {
		var sd model.StrategyDef
		if err := json.Unmarshal([]byte(strategyJSON), &sd); err == nil && sd.Kind != "" {
			a.StrategyDef = &sd
		}
	}
	if inputSchemaJSON != "" && inputSchemaJSON != "null" {
		var schema variable.Schema
		if err := json.Unmarshal([]byte(inputSchemaJSON), &schema); err == nil {
			a.InputSchema = &schema
		}
	}
	if outputSchemaJSON != "" && outputSchemaJSON != "null" {
		var schema variable.Schema
		if err := json.Unmarshal([]byte(outputSchemaJSON), &schema); err == nil {
			a.OutputSchema = &schema
		}
	}
}

// --- Conversation operations ---

func (s *SQLiteStore) ListConversations(ctx context.Context, agentID string, opts model.ListOptions, filters ...model.ListFilter) ([]*model.Conversation, *model.ListResult, error) {
	limit := opts.EffectiveLimit()
	runtimeID := model.ApplyFilters(filters)

	query := `SELECT id, agent_id, runtime_id, variables, status, created_at, updated_at FROM conversations WHERE agent_id = ?`
	args := []any{agentID}
	if runtimeID != "" {
		query += ` AND runtime_id = ?`
		args = append(args, runtimeID)
	}
	if opts.Cursor != "" {
		cursorTime, cursorID, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, nil, err
		}
		query += ` AND (created_at, id) < (?, ?)`
		args = append(args, cursorTime, cursorID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var convs []*model.Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, nil, err
		}
		convs = append(convs, c)
	}

	lr := &model.ListResult{}
	if len(convs) > limit {
		convs = convs[:limit]
		lr.HasMore = true
		last := convs[len(convs)-1]
		lr.NextCursor = encodeCursor(timeStr(last.CreatedAt), last.ID)
	}
	return convs, lr, rows.Err()
}

func (s *SQLiteStore) GetConversation(ctx context.Context, id string) (*model.Conversation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, agent_id, runtime_id, variables, status, created_at, updated_at FROM conversations WHERE id = ?`, id)
	var (
		c            model.Conversation
		varsJSON     sql.NullString
		status       string
		created, upd string
	)
	if err := row.Scan(&c.ID, &c.AgentID, &c.RuntimeID, &varsJSON, &status, &created, &upd); err != nil {
		if err == sql.ErrNoRows {
			return nil, errdefs.NotFoundf("conversation %s not found", id)
		}
		return nil, fmt.Errorf("store: get conversation: %w", err)
	}
	c.Status = model.ConversationStatus(status)
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(upd)
	if varsJSON.Valid && varsJSON.String != "{}" {
		_ = json.Unmarshal([]byte(varsJSON.String), &c.Variables)
	}
	return &c, nil
}

func (s *SQLiteStore) CreateConversation(ctx context.Context, conv *model.Conversation) (*model.Conversation, error) {
	if conv.ID == "" {
		conv.ID = xid.New().String()
	}
	now := time.Now()
	conv.CreatedAt = now
	conv.UpdatedAt = now
	if conv.Status == "" {
		conv.Status = model.ConvActive
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations (id, agent_id, runtime_id, variables, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		conv.ID, conv.AgentID, conv.RuntimeID, marshalJSON(conv.Variables),
		string(conv.Status), timeStr(conv.CreatedAt), timeStr(conv.UpdatedAt))
	if err != nil {
		return nil, fmt.Errorf("store: create conversation: %w", err)
	}
	return conv, nil
}

func (s *SQLiteStore) UpdateConversation(ctx context.Context, conv *model.Conversation) (*model.Conversation, error) {
	conv.UpdatedAt = time.Now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET variables=?, status=?, updated_at=? WHERE id=?`,
		marshalJSON(conv.Variables), string(conv.Status), timeStr(conv.UpdatedAt), conv.ID)
	if err != nil {
		return nil, fmt.Errorf("store: update conversation: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, errdefs.NotFoundf("conversation %s not found", conv.ID)
	}
	return conv, nil
}

func scanConversation(rows *sql.Rows) (*model.Conversation, error) {
	var (
		c            model.Conversation
		varsJSON     sql.NullString
		status       string
		created, upd string
	)
	if err := rows.Scan(&c.ID, &c.AgentID, &c.RuntimeID, &varsJSON, &status, &created, &upd); err != nil {
		return nil, fmt.Errorf("store: scan conversation: %w", err)
	}
	c.Status = model.ConversationStatus(status)
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(upd)
	if varsJSON.Valid && varsJSON.String != "{}" {
		_ = json.Unmarshal([]byte(varsJSON.String), &c.Variables)
	}
	return &c, nil
}

// --- Message operations ---

func (s *SQLiteStore) GetMessages(ctx context.Context, conversationID string) ([]*model.Message, error) {
	return s.GetRecentMessages(ctx, conversationID, 0)
}

func (s *SQLiteStore) GetRecentMessages(ctx context.Context, conversationID string, limit int) ([]*model.Message, error) {
	var query string
	var args []any
	if limit > 0 {
		query = `SELECT id, conversation_id, role, parts, token_count, created_at
			FROM (SELECT *, rowid AS _r FROM messages WHERE conversation_id = ? ORDER BY created_at DESC, _r DESC LIMIT ?)
			ORDER BY created_at ASC, _r ASC`
		args = []any{conversationID, limit}
	} else {
		query = `SELECT id, conversation_id, role, parts, token_count, created_at
			FROM messages WHERE conversation_id = ? ORDER BY created_at ASC, rowid ASC`
		args = []any{conversationID}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: get messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var msgs []*model.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (s *SQLiteStore) SaveMessage(ctx context.Context, msg *model.Message) error {
	if msg.ID == "" {
		msg.ID = xid.New().String()
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (id, conversation_id, role, parts, token_count, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.ConversationID, string(msg.Role),
		marshalJSON(msg.Parts), msg.TokenCount, timeStr(msg.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: save message: %w", err)
	}
	return nil
}

func scanMessage(rows *sql.Rows) (*model.Message, error) {
	var (
		m                   model.Message
		role, partsJSON, ts string
	)
	if err := rows.Scan(&m.ID, &m.ConversationID, &role, &partsJSON, &m.TokenCount, &ts); err != nil {
		return nil, fmt.Errorf("store: scan message: %w", err)
	}
	m.Role = model.Role(role)
	m.CreatedAt = parseTime(ts)
	if partsJSON != "" && partsJSON != "[]" {
		_ = json.Unmarshal([]byte(partsJSON), &m.Parts)
	}
	return &m, nil
}

// --- Workflow run operations ---

func (s *SQLiteStore) SaveWorkflowRun(ctx context.Context, run *model.WorkflowRun) error {
	if run.ID == "" {
		run.ID = xid.New().String()
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workflow_runs (id, agent_id, actor_id, conversation_id, input, output, inputs, outputs, status, usage, elapsed_ms, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET output=excluded.output, outputs=excluded.outputs, status=excluded.status, usage=excluded.usage, elapsed_ms=excluded.elapsed_ms`,
		run.ID, run.AgentID, run.ActorID, run.ConversationID,
		run.Input, run.Output,
		marshalJSON(run.Inputs), marshalJSON(run.Outputs),
		run.Status, marshalJSON(run.Usage), run.ElapsedMs,
		timeStr(run.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: save workflow run: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetWorkflowRun(ctx context.Context, id string) (*model.WorkflowRun, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, agent_id, actor_id, conversation_id, input, output, inputs, outputs, status, usage, elapsed_ms, created_at
		 FROM workflow_runs WHERE id = ?`, id)
	run, err := scanWorkflowRunRow(row)
	if err == sql.ErrNoRows {
		return nil, errdefs.NotFoundf("workflow_run %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get workflow run: %w", err)
	}
	return run, nil
}

func (s *SQLiteStore) ListWorkflowRuns(ctx context.Context, agentID string, opts model.ListOptions) ([]*model.WorkflowRun, *model.ListResult, error) {
	limit := opts.EffectiveLimit()

	query := `SELECT id, agent_id, actor_id, conversation_id, input, output, inputs, outputs, status, usage, elapsed_ms, created_at
		 FROM workflow_runs WHERE agent_id = ?`
	args := []any{agentID}
	if opts.Cursor != "" {
		cursorTime, cursorID, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, nil, err
		}
		query += ` AND (created_at, id) < (?, ?)`
		args = append(args, cursorTime, cursorID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list workflow runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var runs []*model.WorkflowRun
	for rows.Next() {
		run, err := scanWorkflowRun(rows)
		if err != nil {
			return nil, nil, err
		}
		runs = append(runs, run)
	}
	lr := &model.ListResult{}
	if len(runs) > limit {
		runs = runs[:limit]
		lr.HasMore = true
		last := runs[len(runs)-1]
		lr.NextCursor = encodeCursor(timeStr(last.CreatedAt), last.ID)
	}
	return runs, lr, rows.Err()
}

func scanWorkflowRun(rows *sql.Rows) (*model.WorkflowRun, error) {
	var (
		run                                model.WorkflowRun
		inputsJSON, outputsJSON, usageJSON sql.NullString
		ts                                 string
	)
	if err := rows.Scan(&run.ID, &run.AgentID, &run.ActorID, &run.ConversationID,
		&run.Input, &run.Output, &inputsJSON, &outputsJSON,
		&run.Status, &usageJSON, &run.ElapsedMs, &ts); err != nil {
		return nil, fmt.Errorf("store: scan workflow run: %w", err)
	}
	run.CreatedAt = parseTime(ts)
	if inputsJSON.Valid {
		_ = json.Unmarshal([]byte(inputsJSON.String), &run.Inputs)
	}
	if outputsJSON.Valid {
		_ = json.Unmarshal([]byte(outputsJSON.String), &run.Outputs)
	}
	if usageJSON.Valid {
		var u model.TokenUsage
		if json.Unmarshal([]byte(usageJSON.String), &u) == nil {
			run.Usage = &u
		}
	}
	return &run, nil
}

func scanWorkflowRunRow(row *sql.Row) (*model.WorkflowRun, error) {
	var (
		run                                model.WorkflowRun
		inputsJSON, outputsJSON, usageJSON sql.NullString
		ts                                 string
	)
	if err := row.Scan(&run.ID, &run.AgentID, &run.ActorID, &run.ConversationID,
		&run.Input, &run.Output, &inputsJSON, &outputsJSON,
		&run.Status, &usageJSON, &run.ElapsedMs, &ts); err != nil {
		return nil, err
	}
	run.CreatedAt = parseTime(ts)
	if inputsJSON.Valid {
		_ = json.Unmarshal([]byte(inputsJSON.String), &run.Inputs)
	}
	if outputsJSON.Valid {
		_ = json.Unmarshal([]byte(outputsJSON.String), &run.Outputs)
	}
	if usageJSON.Valid {
		var u model.TokenUsage
		if json.Unmarshal([]byte(usageJSON.String), &u) == nil {
			run.Usage = &u
		}
	}
	return &run, nil
}

// --- Execution event operations ---

func (s *SQLiteStore) SaveExecutionEvent(ctx context.Context, ev *model.ExecutionEvent) error {
	if ev.ID == "" {
		ev.ID = xid.New().String()
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO execution_events (id, run_id, node_id, type, payload, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.RunID, ev.NodeID, ev.Type, marshalJSON(ev.Payload), timeStr(ev.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: save execution event: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListExecutionEvents(ctx context.Context, runID string) ([]*model.ExecutionEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, node_id, type, payload, created_at FROM execution_events WHERE run_id = ? ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list execution events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []*model.ExecutionEvent
	for rows.Next() {
		var (
			ev          model.ExecutionEvent
			payloadJSON string
			ts          string
		)
		if err := rows.Scan(&ev.ID, &ev.RunID, &ev.NodeID, &ev.Type, &payloadJSON, &ts); err != nil {
			return nil, fmt.Errorf("store: scan execution event: %w", err)
		}
		ev.CreatedAt = parseTime(ts)
		if payloadJSON != "" && payloadJSON != "{}" {
			_ = json.Unmarshal([]byte(payloadJSON), &ev.Payload)
		}
		events = append(events, &ev)
	}
	return events, rows.Err()
}

// --- Kanban card operations ---

func (s *SQLiteStore) SaveKanbanCard(ctx context.Context, card *model.KanbanCard) error {
	if card.CreatedAt.IsZero() {
		card.CreatedAt = time.Now()
	}
	if card.UpdatedAt.IsZero() {
		card.UpdatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kanban_cards (id, runtime_id, type, status, producer, consumer, target_agent_id, query, output, error, run_id, meta, payload, elapsed_ms, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET status=excluded.status, consumer=excluded.consumer, output=excluded.output, error=excluded.error, run_id=excluded.run_id, elapsed_ms=excluded.elapsed_ms, updated_at=excluded.updated_at`,
		card.ID, card.RuntimeID, card.Type, card.Status, card.Producer, card.Consumer,
		card.TargetAgentID, card.Query, card.Output, card.Error, card.RunID,
		marshalJSON(card.Meta), card.ElapsedMs, timeStr(card.CreatedAt), timeStr(card.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: save kanban card: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListKanbanCards(ctx context.Context, runtimeID string) ([]*model.KanbanCard, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, runtime_id, type, status, producer, consumer, target_agent_id, query, output, error, run_id, meta, elapsed_ms, created_at, updated_at
		 FROM kanban_cards WHERE runtime_id = ? ORDER BY created_at ASC`, runtimeID)
	if err != nil {
		return nil, fmt.Errorf("store: list kanban cards: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var cards []*model.KanbanCard
	for rows.Next() {
		var (
			card             model.KanbanCard
			metaJSON         sql.NullString
			created, updated string
		)
		if err := rows.Scan(&card.ID, &card.RuntimeID, &card.Type, &card.Status, &card.Producer, &card.Consumer,
			&card.TargetAgentID, &card.Query, &card.Output, &card.Error, &card.RunID,
			&metaJSON, &card.ElapsedMs, &created, &updated); err != nil {
			return nil, fmt.Errorf("store: scan kanban card: %w", err)
		}
		card.CreatedAt = parseTime(created)
		card.UpdatedAt = parseTime(updated)
		if metaJSON.Valid && metaJSON.String != "{}" {
			_ = json.Unmarshal([]byte(metaJSON.String), &card.Meta)
		}
		cards = append(cards, &card)
	}
	return cards, rows.Err()
}

func (s *SQLiteStore) DeleteKanbanCards(ctx context.Context, runtimeID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kanban_cards WHERE runtime_id = ?`, runtimeID)
	if err != nil {
		return fmt.Errorf("store: delete kanban cards: %w", err)
	}
	return nil
}

// --- Template operations ---

func (s *SQLiteStore) ListTemplates(ctx context.Context) ([]*model.Template, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, label, description, category, parameters, graph_def, is_builtin, created_at, updated_at
		 FROM templates ORDER BY is_builtin DESC, name ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list templates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var templates []*model.Template
	for rows.Next() {
		var t model.Template
		var created, updated string
		if err := rows.Scan(&t.Name, &t.Label, &t.Description, &t.Category,
			&t.Parameters, &t.GraphDef, &t.IsBuiltin, &created, &updated); err != nil {
			return nil, fmt.Errorf("store: scan template: %w", err)
		}
		t.CreatedAt = parseTime(created)
		t.UpdatedAt = parseTime(updated)
		templates = append(templates, &t)
	}
	return templates, rows.Err()
}

func (s *SQLiteStore) SaveTemplate(ctx context.Context, t *model.Template) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	t.UpdatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO templates (name, label, description, category, parameters, graph_def, is_builtin, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET label=excluded.label, description=excluded.description, category=excluded.category,
		 parameters=excluded.parameters, graph_def=excluded.graph_def, updated_at=excluded.updated_at`,
		t.Name, t.Label, t.Description, t.Category, t.Parameters, t.GraphDef,
		t.IsBuiltin, timeStr(t.CreatedAt), timeStr(t.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: save template: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeleteTemplate(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM templates WHERE name = ? AND is_builtin = 0`, name)
	if err != nil {
		return fmt.Errorf("store: delete template: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return errdefs.NotFoundf("template %s not found", name)
	}
	return nil
}

// --- Dataset operations ---

func (s *SQLiteStore) ListDatasets(ctx context.Context) ([]*model.Dataset, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id, d.agent_id, d.name, d.description, d.document_count, d.l0_abstract, d.created_at, d.updated_at
		 FROM datasets d ORDER BY d.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list datasets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var datasets []*model.Dataset
	for rows.Next() {
		var ds model.Dataset
		var ts, uts string
		if err := rows.Scan(&ds.ID, &ds.AgentID, &ds.Name, &ds.Description, &ds.DocumentCount, &ds.L0Abstract, &ts, &uts); err != nil {
			return nil, fmt.Errorf("store: scan dataset: %w", err)
		}
		ds.CreatedAt = parseTime(ts)
		ds.UpdatedAt = parseTime(uts)
		datasets = append(datasets, &ds)
	}
	return datasets, rows.Err()
}

func (s *SQLiteStore) CreateDataset(ctx context.Context, ds *model.Dataset) (*model.Dataset, error) {
	if ds.ID == "" {
		ds.ID = xid.New().String()
	}
	now := time.Now()
	ds.CreatedAt = now
	ds.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO datasets (id, agent_id, name, description, document_count, l0_abstract, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ds.ID, ds.AgentID, ds.Name, ds.Description, ds.DocumentCount, ds.L0Abstract, timeStr(ds.CreatedAt), timeStr(ds.UpdatedAt))
	if err != nil {
		return nil, fmt.Errorf("store: create dataset: %w", err)
	}
	return ds, nil
}

func (s *SQLiteStore) GetDataset(ctx context.Context, id string) (*model.Dataset, error) {
	var ds model.Dataset
	var ts, uts string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, agent_id, name, description, document_count, l0_abstract, created_at, updated_at FROM datasets WHERE id = ?`, id).
		Scan(&ds.ID, &ds.AgentID, &ds.Name, &ds.Description, &ds.DocumentCount, &ds.L0Abstract, &ts, &uts)
	if err == sql.ErrNoRows {
		return nil, errdefs.NotFoundf("dataset %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get dataset: %w", err)
	}
	ds.CreatedAt = parseTime(ts)
	ds.UpdatedAt = parseTime(uts)
	return &ds, nil
}

func (s *SQLiteStore) DeleteDataset(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM dataset_documents WHERE dataset_id = ?`, id); err != nil {
			return fmt.Errorf("store: delete dataset documents: %w", err)
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM datasets WHERE id = ?`, id)
		return err
	})
}

func (s *SQLiteStore) AddDocument(ctx context.Context, datasetID, name, content string) (*model.DatasetDocument, error) {
	doc := &model.DatasetDocument{
		ID:               xid.New().String(),
		DatasetID:        datasetID,
		Name:             name,
		Content:          content,
		ProcessingStatus: model.ProcessingPending,
		CreatedAt:        time.Now(),
	}
	return doc, s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO dataset_documents (id, dataset_id, name, content, processing_status, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			doc.ID, doc.DatasetID, doc.Name, doc.Content, string(doc.ProcessingStatus), timeStr(doc.CreatedAt))
		if err != nil {
			return fmt.Errorf("store: add document: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE datasets SET document_count = (SELECT COUNT(*) FROM dataset_documents WHERE dataset_id = ?), updated_at = ? WHERE id = ?`,
			datasetID, timeStr(time.Now()), datasetID)
		return err
	})
}

func (s *SQLiteStore) GetDocument(ctx context.Context, datasetID, docID string) (*model.DatasetDocument, error) {
	var doc model.DatasetDocument
	var ts, ps string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, dataset_id, name, content, chunk_count, l0_abstract, l1_overview, processing_status, created_at
		 FROM dataset_documents WHERE id = ? AND dataset_id = ?`, docID, datasetID).
		Scan(&doc.ID, &doc.DatasetID, &doc.Name, &doc.Content,
			&doc.ChunkCount, &doc.L0Abstract, &doc.L1Overview, &ps, &ts)
	if err != nil {
		return nil, fmt.Errorf("store: get document: %w", err)
	}
	doc.ProcessingStatus = model.ProcessingStatus(ps)
	doc.CreatedAt = parseTime(ts)
	return &doc, nil
}

func (s *SQLiteStore) ListDocuments(ctx context.Context, datasetID string) ([]*model.DatasetDocument, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, dataset_id, name, content, chunk_count, l0_abstract, l1_overview, processing_status, created_at
		 FROM dataset_documents WHERE dataset_id = ? ORDER BY created_at ASC`, datasetID)
	if err != nil {
		return nil, fmt.Errorf("store: list documents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var docs []*model.DatasetDocument
	for rows.Next() {
		var doc model.DatasetDocument
		var ts, ps string
		if err := rows.Scan(&doc.ID, &doc.DatasetID, &doc.Name, &doc.Content,
			&doc.ChunkCount, &doc.L0Abstract, &doc.L1Overview, &ps, &ts); err != nil {
			return nil, fmt.Errorf("store: scan document: %w", err)
		}
		doc.ProcessingStatus = model.ProcessingStatus(ps)
		doc.CreatedAt = parseTime(ts)
		docs = append(docs, &doc)
	}
	return docs, rows.Err()
}

func (s *SQLiteStore) DeleteDocument(ctx context.Context, datasetID, docID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM dataset_documents WHERE id = ? AND dataset_id = ?`, docID, datasetID)
		if err != nil {
			return fmt.Errorf("store: delete document: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errdefs.NotFoundf("document %s not found", docID)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE datasets SET document_count = (SELECT COUNT(*) FROM dataset_documents WHERE dataset_id = ?), updated_at = ? WHERE id = ?`,
			datasetID, timeStr(time.Now()), datasetID)
		return err
	})
}

// UpdateDocumentStats updates only the non-nil fields of patch. Returns
// NotFound when the document row is gone (typical race with delete).
func (s *SQLiteStore) UpdateDocumentStats(ctx context.Context, datasetID, docID string, patch model.DocumentStatsPatch) error {
	setParts := make([]string, 0, 4)
	args := make([]any, 0, 6)
	if patch.ChunkCount != nil {
		setParts = append(setParts, "chunk_count = ?")
		args = append(args, *patch.ChunkCount)
	}
	if patch.L0Abstract != nil {
		setParts = append(setParts, "l0_abstract = ?")
		args = append(args, *patch.L0Abstract)
	}
	if patch.L1Overview != nil {
		setParts = append(setParts, "l1_overview = ?")
		args = append(args, *patch.L1Overview)
	}
	if patch.ProcessingStatus != nil {
		setParts = append(setParts, "processing_status = ?")
		args = append(args, string(*patch.ProcessingStatus))
	}
	if len(setParts) == 0 {
		return nil
	}
	args = append(args, docID, datasetID)
	query := fmt.Sprintf("UPDATE dataset_documents SET %s WHERE id = ? AND dataset_id = ?",
		strings.Join(setParts, ", "))
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: update document stats: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errdefs.NotFoundf("document %s not found", docID)
	}
	return nil
}

// UpdateDatasetAbstract overwrites the dataset-level L0 abstract and
// bumps updated_at. Empty datasets (no row) return NotFound so callers
// can treat the rollup as a no-op.
func (s *SQLiteStore) UpdateDatasetAbstract(ctx context.Context, datasetID, abstract string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE datasets SET l0_abstract = ?, updated_at = ? WHERE id = ?`,
		abstract, timeStr(time.Now()), datasetID)
	if err != nil {
		return fmt.Errorf("store: update dataset abstract: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errdefs.NotFoundf("dataset %s not found", datasetID)
	}
	return nil
}

// --- Graph version operations ---

func (s *SQLiteStore) ListGraphVersions(ctx context.Context, agentID string) ([]*model.GraphVersion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, agent_id, version, graph_def, description, checksum, created_by, published_at, created_at
		 FROM graph_versions WHERE agent_id = ? ORDER BY version DESC`, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: list graph versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var versions []*model.GraphVersion
	for rows.Next() {
		v, err := scanGraphVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

func (s *SQLiteStore) GetGraphVersion(ctx context.Context, agentID string, version int) (*model.GraphVersion, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, agent_id, version, graph_def, description, checksum, created_by, published_at, created_at
		 FROM graph_versions WHERE agent_id = ? AND version = ?`, agentID, version)
	v, err := scanGraphVersionRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, errdefs.NotFoundf("graph_version %s/v%d not found", agentID, version)
		}
		return nil, fmt.Errorf("store: get graph version: %w", err)
	}
	return v, nil
}

func (s *SQLiteStore) SaveGraphVersion(ctx context.Context, gv *model.GraphVersion) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO graph_versions (id, agent_id, version, graph_def, description, checksum, created_by, published_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   graph_def=excluded.graph_def, description=excluded.description, checksum=excluded.checksum,
		   created_by=excluded.created_by, published_at=excluded.published_at`,
		gv.ID, gv.AgentID, gv.Version, marshalJSON(gv.GraphDef), gv.Description,
		gv.Checksum, gv.CreatedBy, timeStrPtr(gv.PublishedAt), timeStr(gv.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: save graph version: %w", err)
	}
	return nil
}

func (s *SQLiteStore) PublishGraphVersion(ctx context.Context, agentID string, def *model.GraphDefinition, description string) (*model.GraphVersion, error) {
	var maxVersion int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM graph_versions WHERE agent_id = ?`, agentID).Scan(&maxVersion); err != nil {
		return nil, fmt.Errorf("store: max graph version: %w", err)
	}

	defJSON := marshalJSON(def)
	now := time.Now()
	v := &model.GraphVersion{
		ID:          xid.New().String(),
		AgentID:     agentID,
		Version:     maxVersion + 1,
		GraphDef:    def,
		Description: description,
		Checksum:    checksumJSON(defJSON),
		PublishedAt: &now,
		CreatedAt:   now,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO graph_versions (id, agent_id, version, graph_def, description, checksum, created_by, published_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.AgentID, v.Version, defJSON, v.Description,
		v.Checksum, v.CreatedBy, timeStrPtr(v.PublishedAt), timeStr(v.CreatedAt))
	if err != nil {
		return nil, fmt.Errorf("store: publish graph version: %w", err)
	}
	return v, nil
}

func (s *SQLiteStore) GetLatestPublishedVersion(ctx context.Context, agentID string) (*model.GraphVersion, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, agent_id, version, graph_def, description, checksum, created_by, published_at, created_at
		 FROM graph_versions WHERE agent_id = ? AND published_at IS NOT NULL AND published_at != ''
		 ORDER BY version DESC LIMIT 1`, agentID)
	v, err := scanGraphVersionRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, errdefs.NotFoundf("graph_version %s not found", agentID)
		}
		return nil, fmt.Errorf("store: get latest graph version: %w", err)
	}
	return v, nil
}

func (s *SQLiteStore) UpdateVersionLock(ctx context.Context, agentID string, expectedChecksum string, newDef *model.GraphDefinition) error {
	newDefJSON := marshalJSON(newDef)
	newChecksum := checksumJSON(newDefJSON)

	result, err := s.db.ExecContext(ctx, `
		UPDATE graph_versions
		SET graph_def = ?, checksum = ?, published_at = NULL
		WHERE agent_id = ? AND (published_at IS NULL OR published_at = '') AND checksum = ?`,
		newDefJSON, newChecksum, agentID, expectedChecksum)
	if err != nil {
		return fmt.Errorf("store: update version lock: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return errdefs.Conflictf("draft has been modified by another user (checksum mismatch)")
	}
	return nil
}

// --- Graph operation history ---

func (s *SQLiteStore) SaveGraphOperation(ctx context.Context, op *model.GraphOperation) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO graph_operations (id, agent_id, type, node_id, edge_from, edge_to, graph_def, description, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, op.AgentID, op.Type, nullStr(op.NodeID), nullStr(op.EdgeFrom), nullStr(op.EdgeTo),
		marshalJSON(op.GraphDef), op.Description, nullStr(op.CreatedBy), timeStr(op.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: save graph operation: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListGraphOperations(ctx context.Context, agentID string, opts model.ListOptions) ([]*model.GraphOperation, *model.ListResult, error) {
	limit := opts.EffectiveLimit()

	var query string
	var args []any

	if opts.Cursor != "" {
		cursorTime, cursorID, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, nil, fmt.Errorf("store: list graph operations: %w", err)
		}
		query = `SELECT id, agent_id, type, node_id, edge_from, edge_to, graph_def, description, created_by, created_at
			 FROM graph_operations
			 WHERE agent_id = ? AND (created_at < ? OR (created_at = ? AND id < ?))
			 ORDER BY created_at DESC, id DESC
			 LIMIT ?`
		args = []any{agentID, cursorTime, cursorTime, cursorID, limit + 1}
	} else {
		query = `SELECT id, agent_id, type, node_id, edge_from, edge_to, graph_def, description, created_by, created_at
			 FROM graph_operations
			 WHERE agent_id = ?
			 ORDER BY created_at DESC, id DESC
			 LIMIT ?`
		args = []any{agentID, limit + 1}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list graph operations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var operations []*model.GraphOperation
	for rows.Next() {
		var op model.GraphOperation
		var nodeID, edgeFrom, edgeTo, createdBy sql.NullString
		var graphDefJSON string
		if err := rows.Scan(&op.ID, &op.AgentID, &op.Type, &nodeID, &edgeFrom, &edgeTo, &graphDefJSON, &op.Description, &createdBy, &op.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("store: scan graph operation: %w", err)
		}
		op.NodeID = nodeID.String
		op.EdgeFrom = edgeFrom.String
		op.EdgeTo = edgeTo.String
		op.CreatedBy = createdBy.String
		if graphDefJSON != "" && graphDefJSON != "{}" {
			if err := json.Unmarshal([]byte(graphDefJSON), &op.GraphDef); err != nil {
				telemetry.Warn(ctx, "store: invalid graph_def JSON in graph_operations",
					otellog.String("op_id", op.ID),
					otellog.String("error", err.Error()))
			}
		}
		operations = append(operations, &op)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: list graph operations: %w", err)
	}

	hasMore := len(operations) > limit
	if hasMore {
		operations = operations[:limit]
	}
	var nextCursor string
	if hasMore && len(operations) > 0 {
		lastOp := operations[len(operations)-1]
		nextCursor = encodeCursor(timeStr(lastOp.CreatedAt), lastOp.ID)
	}
	return operations, &model.ListResult{HasMore: hasMore, NextCursor: nextCursor}, nil
}

func scanGraphVersion(rows *sql.Rows) (*model.GraphVersion, error) {
	var (
		v                   model.GraphVersion
		graphDefJSON        string
		checksum, createdBy sql.NullString
		published           sql.NullString
		created             string
	)
	if err := rows.Scan(&v.ID, &v.AgentID, &v.Version, &graphDefJSON, &v.Description,
		&checksum, &createdBy, &published, &created); err != nil {
		return nil, fmt.Errorf("store: scan graph version: %w", err)
	}
	v.Checksum = checksum.String
	v.CreatedBy = createdBy.String
	if published.Valid && published.String != "" {
		t := parseTime(published.String)
		v.PublishedAt = &t
	}
	v.CreatedAt = parseTime(created)
	if graphDefJSON != "" {
		var gd model.GraphDefinition
		if json.Unmarshal([]byte(graphDefJSON), &gd) == nil {
			v.GraphDef = &gd
		}
	}
	return &v, nil
}

func scanGraphVersionRow(row *sql.Row) (*model.GraphVersion, error) {
	var (
		v                   model.GraphVersion
		graphDefJSON        string
		checksum, createdBy sql.NullString
		published           sql.NullString
		created             string
	)
	if err := row.Scan(&v.ID, &v.AgentID, &v.Version, &graphDefJSON, &v.Description,
		&checksum, &createdBy, &published, &created); err != nil {
		return nil, err
	}
	v.Checksum = checksum.String
	v.CreatedBy = createdBy.String
	if published.Valid && published.String != "" {
		t := parseTime(published.String)
		v.PublishedAt = &t
	}
	v.CreatedAt = parseTime(created)
	if graphDefJSON != "" {
		var gd model.GraphDefinition
		if json.Unmarshal([]byte(graphDefJSON), &gd) == nil {
			v.GraphDef = &gd
		}
	}
	return &v, nil
}

func checksumJSON(jsonStr string) string {
	h := sha256.Sum256([]byte(jsonStr))
	return fmt.Sprintf("%x", h)
}

// --- Stats operations ---

func (s *SQLiteStore) GetStats(ctx context.Context) (*model.StatsOverview, error) {
	var st model.StatsOverview
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents`).Scan(&st.TotalAgents); err != nil {
		return nil, fmt.Errorf("store: stats agents: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations`).Scan(&st.TotalConversations); err != nil {
		return nil, fmt.Errorf("store: stats conversations: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_runs`).Scan(&st.TotalRuns); err != nil {
		return nil, fmt.Errorf("store: stats workflow_runs: %w", err)
	}
	return &st, nil
}

func (s *SQLiteStore) GetRunStats(ctx context.Context, agentID string) (*model.RunStats, error) {
	var rs model.RunStats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_runs WHERE agent_id = ?`, agentID).Scan(&rs.TotalRuns); err != nil {
		return nil, fmt.Errorf("store: run stats total: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_runs WHERE agent_id = ? AND status = 'completed'`, agentID).Scan(&rs.CompletedRuns); err != nil {
		return nil, fmt.Errorf("store: run stats completed: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_runs WHERE agent_id = ? AND status = 'failed'`, agentID).Scan(&rs.FailedRuns); err != nil {
		return nil, fmt.Errorf("store: run stats failed: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(AVG(elapsed_ms), 0) FROM workflow_runs WHERE agent_id = ?`, agentID).Scan(&rs.AvgElapsedMs); err != nil {
		return nil, fmt.Errorf("store: run stats avg elapsed: %w", err)
	}
	return &rs, nil
}

func (s *SQLiteStore) ListDailyRunStats(ctx context.Context, agentID string, days int) ([]*model.DailyRunStats, error) {
	if days <= 0 {
		days = 30
	}
	q := `SELECT date(created_at) as d, COUNT(*) as cnt, COALESCE(AVG(elapsed_ms), 0) as avg_ms
		  FROM workflow_runs WHERE created_at >= date('now', '-' || ? || ' days')`
	args := []any{days}
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	q += ` GROUP BY d ORDER BY d ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list daily run stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*model.DailyRunStats
	for rows.Next() {
		var ds model.DailyRunStats
		if err := rows.Scan(&ds.Date, &ds.Count, &ds.AvgElapsedMs); err != nil {
			return nil, fmt.Errorf("store: scan daily run stats: %w", err)
		}
		out = append(out, &ds)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetMonitoringSummary(ctx context.Context, agentID string, since time.Time) (*model.MonitoringSummary, error) {
	sinceStr := timeStr(since)
	q := `SELECT COUNT(*),
		SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END)
		FROM workflow_runs WHERE created_at >= ?`
	args := []any{sinceStr}
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	row := s.db.QueryRowContext(ctx, q, args...)
	var total, success, failed sql.NullInt64
	if err := row.Scan(&total, &success, &failed); err != nil {
		return nil, fmt.Errorf("store: monitoring summary: %w", err)
	}

	elapsed, err := s.listElapsedInWindow(ctx, agentID, since)
	if err != nil {
		return nil, err
	}
	p50 := percentile(elapsed, 0.50)
	p95 := percentile(elapsed, 0.95)
	p99 := percentile(elapsed, 0.99)

	runTotal := total.Int64
	runSuccess := success.Int64
	runFailed := failed.Int64
	var successRate *float64
	var errorRate *float64
	if runTotal > 0 {
		sr := float64(runSuccess) / float64(runTotal)
		er := float64(runFailed) / float64(runTotal)
		successRate = &sr
		errorRate = &er
	}

	return &model.MonitoringSummary{
		WindowStart:  since.UTC(),
		WindowEnd:    time.Now().UTC(),
		RunTotal:     runTotal,
		RunSuccess:   runSuccess,
		RunFailed:    runFailed,
		SuccessRate:  successRate,
		ErrorRate:    errorRate,
		LatencyP50Ms: p50,
		LatencyP95Ms: p95,
		LatencyP99Ms: p99,
	}, nil
}

func (s *SQLiteStore) ListMonitoringTimeseries(ctx context.Context, agentID string, since time.Time, interval time.Duration) ([]*model.MonitoringTimeseriesPoint, error) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	sinceStr := timeStr(since)
	q := `SELECT created_at, status, elapsed_ms FROM workflow_runs WHERE created_at >= ?`
	args := []any{sinceStr}
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	q += ` ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: monitoring timeseries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type agg struct {
		total   int64
		success int64
		failed  int64
		elapsed []int64
	}
	buckets := make(map[time.Time]*agg)
	var bucketOrder []time.Time
	for rows.Next() {
		var createdAt, status string
		var elapsed sql.NullInt64
		if err := rows.Scan(&createdAt, &status, &elapsed); err != nil {
			return nil, fmt.Errorf("store: scan monitoring timeseries: %w", err)
		}
		created := parseTime(createdAt).UTC()
		bucket := created.Truncate(interval)
		a, ok := buckets[bucket]
		if !ok {
			a = &agg{}
			buckets[bucket] = a
			bucketOrder = append(bucketOrder, bucket)
		}
		a.total++
		if status == "completed" {
			a.success++
		}
		if status == "failed" {
			a.failed++
		}
		if elapsed.Valid && elapsed.Int64 > 0 {
			a.elapsed = append(a.elapsed, elapsed.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: monitoring timeseries rows: %w", err)
	}
	sort.Slice(bucketOrder, func(i, j int) bool { return bucketOrder[i].Before(bucketOrder[j]) })
	out := make([]*model.MonitoringTimeseriesPoint, 0, len(bucketOrder))
	minutes := interval.Minutes()
	if minutes <= 0 {
		minutes = 1
	}
	for _, b := range bucketOrder {
		a := buckets[b]
		var successRate *float64
		var errorRate *float64
		var avg *float64
		if a.total > 0 {
			sr := float64(a.success) / float64(a.total)
			er := float64(a.failed) / float64(a.total)
			successRate = &sr
			errorRate = &er
		}
		if len(a.elapsed) > 0 {
			var sum int64
			for _, v := range a.elapsed {
				sum += v
			}
			av := float64(sum) / float64(len(a.elapsed))
			avg = &av
		}
		out = append(out, &model.MonitoringTimeseriesPoint{
			BucketStart:   b,
			RunTotal:      a.total,
			RunSuccess:    a.success,
			RunFailed:     a.failed,
			SuccessRate:   successRate,
			ErrorRate:     errorRate,
			LatencyP50Ms:  percentile(a.elapsed, 0.50),
			LatencyP95Ms:  percentile(a.elapsed, 0.95),
			LatencyP99Ms:  percentile(a.elapsed, 0.99),
			AvgElapsedMs:  avg,
			ThroughputRPM: float64(a.total) / minutes,
		})
	}
	return out, nil
}

func (s *SQLiteStore) GetMonitoringDiagnostics(ctx context.Context, agentID string, since time.Time, limit int) (*model.MonitoringDiagnostics, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	sinceStr := timeStr(since)

	baseWhere := ` WHERE created_at >= ?`
	baseArgs := []any{sinceStr}
	if agentID != "" {
		baseWhere += ` AND agent_id = ?`
		baseArgs = append(baseArgs, agentID)
	}

	topAgentQuery := `SELECT agent_id,
		SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END) AS failed_runs,
		COUNT(*) AS total_runs
		FROM workflow_runs` + baseWhere + `
		GROUP BY agent_id
		HAVING failed_runs > 0
		ORDER BY failed_runs DESC
		LIMIT ?`
	topAgentArgs := append(append([]any{}, baseArgs...), limit)
	topRows, err := s.db.QueryContext(ctx, topAgentQuery, topAgentArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: monitoring diagnostics top agents: %w", err)
	}
	defer func() { _ = topRows.Close() }()
	topAgents := make([]model.MonitoringTopFailedAgent, 0)
	for topRows.Next() {
		var item model.MonitoringTopFailedAgent
		if err := topRows.Scan(&item.AgentID, &item.FailedRuns, &item.TotalRuns); err != nil {
			return nil, fmt.Errorf("store: scan top failed agents: %w", err)
		}
		if item.TotalRuns > 0 {
			rate := float64(item.FailedRuns) / float64(item.TotalRuns)
			item.FailureRate = &rate
		}
		topAgents = append(topAgents, item)
	}
	if err := topRows.Err(); err != nil {
		return nil, fmt.Errorf("store: top failed agents rows: %w", err)
	}

	type failedRun struct {
		runID      string
		agentID    string
		output     string
		outputsRaw string
		elapsedMs  int64
		createdAt  time.Time
	}
	allFailedQuery := `SELECT id, agent_id, output, outputs, elapsed_ms, created_at
		FROM workflow_runs` + baseWhere + ` AND status = 'failed'
		ORDER BY created_at DESC
		LIMIT 5000`
	allFailedRows, err := s.db.QueryContext(ctx, allFailedQuery, baseArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: monitoring diagnostics failed runs: %w", err)
	}
	defer func() { _ = allFailedRows.Close() }()
	allFailed := make([]failedRun, 0, 64)
	for allFailedRows.Next() {
		var run failedRun
		var output sql.NullString
		var outputs sql.NullString
		var elapsed sql.NullInt64
		var createdAt string
		if err := allFailedRows.Scan(&run.runID, &run.agentID, &output, &outputs, &elapsed, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan failed runs: %w", err)
		}
		if output.Valid {
			run.output = output.String
		}
		if outputs.Valid {
			run.outputsRaw = outputs.String
		}
		if elapsed.Valid {
			run.elapsedMs = elapsed.Int64
		}
		run.createdAt = parseTime(createdAt)
		allFailed = append(allFailed, run)
	}
	if err := allFailedRows.Err(); err != nil {
		return nil, fmt.Errorf("store: failed runs rows: %w", err)
	}
	if err := allFailedRows.Close(); err != nil {
		return nil, fmt.Errorf("store: close failed runs rows: %w", err)
	}

	// Collect run IDs that need event-based error code resolution in a single
	// batch query instead of issuing one query per run (N+1).
	var needEventLookup []string
	for i := range allFailed {
		run := &allFailed[i]
		if extractErrorCodeFromOutputs(run.outputsRaw) == "" && extractErrorCodeFromMessage(run.output) == "" {
			needEventLookup = append(needEventLookup, run.runID)
		}
	}
	eventCodes := s.batchLookupErrorCodesFromEvents(ctx, needEventLookup)

	// Resolve error codes once per run and cache both code and message.
	type resolved struct {
		code string
		msg  string
	}
	resolvedRuns := make([]resolved, len(allFailed))
	errorCounts := map[string]int64{}
	for i := range allFailed {
		run := &allFailed[i]
		code := resolveErrorCodeWithCache(run.output, run.outputsRaw, eventCodes[run.runID])
		msg := extractFailureMessage(run.output, run.outputsRaw)
		resolvedRuns[i] = resolved{code: code, msg: msg}
		errorCounts[code]++
	}
	recent := make([]model.MonitoringRecentFailure, 0, min(limit, len(allFailed)))
	for i := 0; i < len(allFailed) && i < limit; i++ {
		run := &allFailed[i]
		r := resolvedRuns[i]
		recent = append(recent, model.MonitoringRecentFailure{
			RunID:     run.runID,
			AgentID:   run.agentID,
			ErrorCode: r.code,
			Message:   trimMessage(r.msg),
			ElapsedMs: run.elapsedMs,
			CreatedAt: run.createdAt,
		})
	}

	type errorItem struct {
		code  string
		count int64
	}
	items := make([]errorItem, 0, len(errorCounts))
	for code, count := range errorCounts {
		items = append(items, errorItem{code: code, count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count == items[j].count {
			return items[i].code < items[j].code
		}
		return items[i].count > items[j].count
	})
	topCodes := make([]model.MonitoringTopErrorCode, 0, min(limit, len(items)))
	for i := 0; i < len(items) && i < limit; i++ {
		topCodes = append(topCodes, model.MonitoringTopErrorCode{
			Code:  items[i].code,
			Count: items[i].count,
		})
	}

	return &model.MonitoringDiagnostics{
		TopFailedAgents: topAgents,
		TopErrorCodes:   topCodes,
		RecentFailures:  recent,
	}, nil
}

// batchLookupErrorCodesFromEvents resolves error codes from execution_events
// for multiple run IDs in a single query, avoiding N+1 DB round-trips.
func (s *SQLiteStore) batchLookupErrorCodesFromEvents(ctx context.Context, runIDs []string) map[string]string {
	result := make(map[string]string, len(runIDs))
	if len(runIDs) == 0 {
		return result
	}
	placeholders := make([]string, len(runIDs))
	args := make([]any, len(runIDs))
	for i, id := range runIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT run_id, type, payload FROM execution_events
		WHERE run_id IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return result
	}
	defer func() { _ = rows.Close() }()

	type eventPayload struct {
		eventType string
		payload   string
	}
	perRun := make(map[string][]eventPayload)
	for rows.Next() {
		var runID, eventType string
		var payload sql.NullString
		if err := rows.Scan(&runID, &eventType, &payload); err != nil {
			continue
		}
		if !payload.Valid || payload.String == "" || payload.String == "{}" {
			continue
		}
		perRun[runID] = append(perRun[runID], eventPayload{
			eventType: strings.ToLower(eventType),
			payload:   payload.String,
		})
	}
	for runID, candidates := range perRun {
		for _, item := range candidates {
			if strings.Contains(item.eventType, "error") || strings.Contains(item.eventType, "fail") {
				if code := extractErrorCodeFromOutputs(item.payload); code != "" {
					result[runID] = code
					break
				}
			}
		}
		if result[runID] != "" {
			continue
		}
		for _, item := range candidates {
			if code := extractErrorCodeFromOutputs(item.payload); code != "" {
				result[runID] = code
				break
			}
		}
	}
	return result
}

// resolveErrorCodeWithCache resolves the error code for a failed run using
// pre-computed event codes, avoiding repeated DB lookups.
func resolveErrorCodeWithCache(output, outputsRaw, eventCode string) string {
	if code := extractErrorCodeFromOutputs(outputsRaw); code != "" {
		return code
	}
	if code := extractErrorCodeFromMessage(output); code != "" {
		return code
	}
	if eventCode != "" {
		return eventCode
	}
	return detectErrorCode(output)
}

func extractFailureMessage(output, outputsRaw string) string {
	if msg := extractMessageFromOutputs(outputsRaw); msg != "" {
		return msg
	}
	return output
}

func extractMessageFromOutputs(outputsRaw string) string {
	m := parseJSONMap(outputsRaw)
	if len(m) == 0 {
		return ""
	}
	if msg := lookupStringByPath(m, "error", "message"); msg != "" {
		return msg
	}
	if msg := lookupStringByPath(m, "message"); msg != "" {
		return msg
	}
	if msg := lookupStringByPath(m, "error_message"); msg != "" {
		return msg
	}
	return ""
}

func extractErrorCodeFromOutputs(outputsRaw string) string {
	m := parseJSONMap(outputsRaw)
	if len(m) == 0 {
		return ""
	}
	if code := lookupStringByPath(m, "error_code"); code != "" {
		return sanitizeErrorCode(code)
	}
	if code := lookupStringByPath(m, "error", "code"); code != "" {
		return sanitizeErrorCode(code)
	}
	if code := lookupStringByPath(m, "code"); code != "" {
		return sanitizeErrorCode(code)
	}
	return ""
}

func parseJSONMap(raw string) map[string]any {
	if raw == "" || raw == "{}" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

func lookupStringByPath(m map[string]any, path ...string) string {
	var cur any = m
	for _, p := range path {
		next, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = next[p]
		if !ok {
			return ""
		}
	}
	if s, ok := cur.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func extractErrorCodeFromMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	if strings.HasPrefix(msg, "{") {
		if code := extractErrorCodeFromOutputs(msg); code != "" {
			return code
		}
	}
	return ""
}

func sanitizeErrorCode(code string) string {
	code = strings.TrimSpace(strings.ToLower(code))
	if code == "" {
		return ""
	}
	return strings.ReplaceAll(code, " ", "_")
}

func (s *SQLiteStore) listElapsedInWindow(ctx context.Context, agentID string, since time.Time) ([]int64, error) {
	sinceStr := timeStr(since)
	q := `SELECT elapsed_ms FROM workflow_runs WHERE created_at >= ? AND elapsed_ms > 0`
	args := []any{sinceStr}
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list elapsed in window: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var v sql.NullInt64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: scan elapsed in window: %w", err)
		}
		if v.Valid && v.Int64 > 0 {
			out = append(out, v.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: elapsed in window rows: %w", err)
	}
	return out, nil
}

func percentile(values []int64, p float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	cp := append([]int64(nil), values...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	if p <= 0 {
		v := float64(cp[0])
		return &v
	}
	if p >= 1 {
		v := float64(cp[len(cp)-1])
		return &v
	}
	rank := p * float64(len(cp)-1)
	lower := int(rank)
	frac := rank - float64(lower)
	v := float64(cp[lower])
	if lower+1 < len(cp) {
		v += frac * float64(cp[lower+1]-cp[lower])
	}
	return &v
}

func detectErrorCode(msg string) string {
	if msg == "" {
		return "unknown"
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline exceeded"):
		return "timeout"
	case strings.Contains(lower, "not found"), strings.Contains(lower, "404"):
		return "not_found"
	case strings.Contains(lower, "forbidden"), strings.Contains(lower, "permission"):
		return "forbidden"
	case strings.Contains(lower, "unauthorized"), strings.Contains(lower, "401"):
		return "unauthorized"
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "429"):
		return "rate_limit"
	case strings.Contains(lower, "validation"), strings.Contains(lower, "invalid"):
		return "validation"
	default:
		return "error"
	}
}

func trimMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > 240 {
		return msg[:240] + "..."
	}
	return msg
}

// --- Provider config operations ---

func (s *SQLiteStore) GetProviderConfig(ctx context.Context, provider string) (*model.ProviderConfig, error) {
	var pc model.ProviderConfig
	var configJSON string
	err := s.db.QueryRowContext(ctx, `SELECT provider, config FROM provider_configs WHERE provider = ?`, provider).
		Scan(&pc.Provider, &configJSON)
	if err == sql.ErrNoRows {
		return nil, errdefs.NotFoundf("provider_config %s not found", provider)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get provider config: %w", err)
	}
	if configJSON != "" {
		_ = json.Unmarshal([]byte(configJSON), &pc.Config)
	}
	return &pc, nil
}

func (s *SQLiteStore) SetProviderConfig(ctx context.Context, pc *model.ProviderConfig) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO provider_configs (provider, config) VALUES (?, ?)
		 ON CONFLICT(provider) DO UPDATE SET config = excluded.config`,
		pc.Provider, marshalJSON(pc.Config))
	if err != nil {
		return fmt.Errorf("store: set provider config: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeleteProviderConfig(ctx context.Context, provider string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM provider_configs WHERE provider = ?`, provider)
	if err != nil {
		return fmt.Errorf("store: delete provider config: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListProviderConfigs(ctx context.Context) ([]*model.ProviderConfig, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, config FROM provider_configs ORDER BY provider`)
	if err != nil {
		return nil, fmt.Errorf("store: list provider configs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var configs []*model.ProviderConfig
	for rows.Next() {
		var pc model.ProviderConfig
		var configJSON string
		if err := rows.Scan(&pc.Provider, &configJSON); err != nil {
			return nil, fmt.Errorf("store: scan provider config: %w", err)
		}
		if configJSON != "" {
			_ = json.Unmarshal([]byte(configJSON), &pc.Config)
		}
		configs = append(configs, &pc)
	}
	return configs, rows.Err()
}

// Compile-time assertions that SQLiteStore (and therefore
// TracingStore, which composes it) satisfies the SDK's optional
// resolver extension interfaces. These are load-bearing: if a method
// signature drifts, the resolver's type assertions would silently
// miss and per-model caps would stop being applied (recreating the
// B1 dead-link bug). Breaking this line is the canary.
var (
	_ llm.ModelConfigStore    = (*SQLiteStore)(nil)
	_ llm.DefaultModelStore   = (*SQLiteStore)(nil)
	_ llm.ProviderConfigStore = (*SQLiteStore)(nil)
)

// --- Model config operations ---
//
// model_configs holds per-model overrides ({Caps, Extra}) keyed by
// (provider, model). The store satisfies llm.ModelConfigStore by
// returning *model.ModelConfig (an alias for *llm.ModelConfig)
// directly from GetModelConfig, so the resolver can walk the typed
// path without conversion.

func (s *SQLiteStore) GetModelConfig(ctx context.Context, provider, mdl string) (*model.ModelConfig, error) {
	var capsJSON, extraJSON string
	err := s.db.QueryRowContext(ctx,
		`SELECT caps, extra FROM model_configs WHERE provider = ? AND model = ?`,
		provider, mdl).Scan(&capsJSON, &extraJSON)
	if err == sql.ErrNoRows {
		return nil, errdefs.NotFoundf("model_config %s/%s not found", provider, mdl)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get model config: %w", err)
	}
	mc := &model.ModelConfig{Provider: provider, Model: mdl}
	if capsJSON != "" {
		_ = json.Unmarshal([]byte(capsJSON), &mc.Caps)
	}
	if extraJSON != "" {
		_ = json.Unmarshal([]byte(extraJSON), &mc.Extra)
	}
	return mc, nil
}

func (s *SQLiteStore) SetModelConfig(ctx context.Context, mc *model.ModelConfig) error {
	if mc == nil || mc.Provider == "" || mc.Model == "" {
		return errdefs.Validationf("provider and model are required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO model_configs (provider, model, caps, extra) VALUES (?, ?, ?, ?)
		 ON CONFLICT(provider, model) DO UPDATE
		 SET caps = excluded.caps, extra = excluded.extra`,
		mc.Provider, mc.Model, marshalJSON(mc.Caps), marshalJSON(mc.Extra))
	if err != nil {
		return fmt.Errorf("store: set model config: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeleteModelConfig(ctx context.Context, provider, mdl string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM model_configs WHERE provider = ? AND model = ?`,
		provider, mdl)
	if err != nil {
		return fmt.Errorf("store: delete model config: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListModelConfigs(ctx context.Context) ([]*model.ModelConfig, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider, model, caps, extra FROM model_configs ORDER BY provider, model`)
	if err != nil {
		return nil, fmt.Errorf("store: list model configs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var configs []*model.ModelConfig
	for rows.Next() {
		var capsJSON, extraJSON string
		mc := &model.ModelConfig{}
		if err := rows.Scan(&mc.Provider, &mc.Model, &capsJSON, &extraJSON); err != nil {
			return nil, fmt.Errorf("store: scan model config: %w", err)
		}
		if capsJSON != "" {
			_ = json.Unmarshal([]byte(capsJSON), &mc.Caps)
		}
		if extraJSON != "" {
			_ = json.Unmarshal([]byte(extraJSON), &mc.Extra)
		}
		configs = append(configs, mc)
	}
	return configs, rows.Err()
}

// --- Default model operations ---
//
// default_model is a singleton row (CHECK(id = 1)). The store
// satisfies llm.DefaultModelStore by returning *model.DefaultModelRef
// from GetDefaultModel; the resolver consults this before falling
// back to WithFallbackModel.

func (s *SQLiteStore) GetDefaultModel(ctx context.Context) (*model.DefaultModelRef, error) {
	ref := &model.DefaultModelRef{}
	err := s.db.QueryRowContext(ctx,
		`SELECT provider, model FROM default_model WHERE id = 1`).
		Scan(&ref.Provider, &ref.Model)
	if err == sql.ErrNoRows {
		return nil, errdefs.NotFoundf("default model not set")
	}
	if err != nil {
		return nil, fmt.Errorf("store: get default model: %w", err)
	}
	return ref, nil
}

func (s *SQLiteStore) SetDefaultModel(ctx context.Context, ref *model.DefaultModelRef) error {
	if ref == nil || ref.Provider == "" || ref.Model == "" {
		return errdefs.Validationf("provider and model are required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO default_model (id, provider, model) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET provider = excluded.provider, model = excluded.model`,
		ref.Provider, ref.Model)
	if err != nil {
		return fmt.Errorf("store: set default model: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ClearDefaultModel(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM default_model WHERE id = 1`)
	if err != nil {
		return fmt.Errorf("store: clear default model: %w", err)
	}
	return nil
}

// --- Owner credential operations ---

func (s *SQLiteStore) GetOwnerCredential(ctx context.Context) (*model.OwnerCredential, error) {
	var cred model.OwnerCredential
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT username, password_hash, created_at, updated_at FROM owner_credential WHERE id = 1`).
		Scan(&cred.Username, &cred.PasswordHash, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, errdefs.NotFoundf("owner credential not initialized")
	}
	if err != nil {
		return nil, fmt.Errorf("store: get owner credential: %w", err)
	}
	cred.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	cred.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &cred, nil
}

func (s *SQLiteStore) SetOwnerCredential(ctx context.Context, cred *model.OwnerCredential) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO owner_credential (id, username, password_hash, created_at, updated_at)
		 VALUES (1, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET username = excluded.username, password_hash = excluded.password_hash, updated_at = excluded.updated_at`,
		cred.Username, cred.PasswordHash, now, now)
	if err != nil {
		return fmt.Errorf("store: set owner credential: %w", err)
	}
	return nil
}

// --- Settings operations ---

func (s *SQLiteStore) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", errdefs.NotFoundf("setting %q not found", key)
	}
	if err != nil {
		return "", fmt.Errorf("store: get setting %q: %w", key, err)
	}
	return value, nil
}

func (s *SQLiteStore) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	if err != nil {
		return fmt.Errorf("store: set setting %q: %w", key, err)
	}
	return nil
}
