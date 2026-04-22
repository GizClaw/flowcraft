package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// handleAdminProjectionStatus answers GET /api/admin/projection/status.
// The response is the snapshot every Manager projector reports (see
// projection.Manager.Status).
func (s *Server) handleAdminProjectionStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) {
		return
	}
	probe := s.deps.ProjectionStatus
	if probe == nil {
		writeJSON(w, http.StatusOK, []ProjectorStatusView{})
		return
	}
	st := probe.Status()
	out := make([]ProjectorStatusView, 0, len(st))
	for _, p := range st {
		out = append(out, ProjectorStatusView{
			Name:                p.Name,
			CheckpointSeq:       p.CheckpointSeq,
			LatestSeq:           p.LatestSeq,
			Lag:                 p.Lag,
			Ready:               p.Ready,
			ConsecutiveFailures: p.ConsecutiveFailures,
			LastError:           p.LastError,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type deadLetterDTO struct {
	ID            int64           `json:"id"`
	ProjectorName string          `json:"projector_name"`
	EventSeq      int64           `json:"event_seq"`
	EventType     string          `json:"event_type"`
	ErrorClass    string          `json:"error_class"`
	ErrorMessage  string          `json:"error_message"`
	Envelope      json.RawMessage `json:"envelope"`
	CreatedAt     string          `json:"created_at"`
	ResolvedAt    string          `json:"resolved_at,omitempty"`
}

// handleAdminDeadLettersList answers GET /api/admin/projection/dead-letters.
// Filters: projector (string), limit (default 50, max 500). Only unresolved
// rows are returned by default; pass include_resolved=1 for the full set.
func (s *Server) handleAdminDeadLettersList(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	projector := q.Get("projector")
	includeResolved := q.Get("include_resolved") == "1"

	rows, err := s.queryDeadLetters(r.Context(), projector, limit, includeResolved)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleAdminDeadLetterReplay answers POST
// /api/admin/projection/dead-letters/{id}/replay. It re-applies the saved
// envelope to the original projector and, on success, marks the row resolved.
// 409 is returned if the row was already resolved.
func (s *Server) handleAdminDeadLetterReplay(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{
			"code":    "bad_request",
			"message": "invalid id",
		}})
		return
	}
	if s.deps.ProjectionReplayer == nil || s.deps.EventLog == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{
			"code":    "not_available",
			"message": "projection replayer not configured",
		}})
		return
	}

	row, err := s.fetchDeadLetter(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if row == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{
			"code":    "not_found",
			"message": "dead-letter row not found",
		}})
		return
	}
	if row.ResolvedAt != "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{
			"code":    "already_resolved",
			"message": "dead-letter already resolved",
		}})
		return
	}

	env, err := decodeDeadLetterEnvelope(row.Envelope)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.deps.ProjectionReplayer.ReplayEvent(r.Context(), s.deps.EventLog, row.ProjectorName, env); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]string{
			"code":    "replay_failed",
			"message": err.Error(),
		}})
		return
	}
	if _, err := s.deps.EventLog.DB().ExecContext(r.Context(),
		`UPDATE dead_letters SET resolved_at=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "replayed", "id": id})
}

func (s *Server) queryDeadLetters(ctx context.Context, projector string, limit int, includeResolved bool) ([]deadLetterDTO, error) {
	if s.deps.EventLog == nil {
		return nil, nil
	}
	q := `SELECT id, projector_name, event_seq, event_type, error_class, error_message,
	             envelope, created_at, COALESCE(resolved_at, '')
	      FROM dead_letters WHERE 1=1`
	args := []any{}
	if !includeResolved {
		q += ` AND resolved_at IS NULL`
	}
	if projector != "" {
		q += ` AND projector_name=?`
		args = append(args, projector)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.deps.EventLog.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]deadLetterDTO, 0)
	for rows.Next() {
		var d deadLetterDTO
		var env []byte
		if err := rows.Scan(&d.ID, &d.ProjectorName, &d.EventSeq, &d.EventType,
			&d.ErrorClass, &d.ErrorMessage, &env, &d.CreatedAt, &d.ResolvedAt); err != nil {
			return nil, err
		}
		d.Envelope = json.RawMessage(env)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Server) fetchDeadLetter(ctx context.Context, id int64) (*deadLetterDTO, error) {
	if s.deps.EventLog == nil {
		return nil, nil
	}
	row := s.deps.EventLog.DB().QueryRowContext(ctx, `
		SELECT id, projector_name, event_seq, event_type, error_class, error_message,
		       envelope, created_at, COALESCE(resolved_at, '')
		FROM dead_letters WHERE id=?`, id)
	var d deadLetterDTO
	var env []byte
	if err := row.Scan(&d.ID, &d.ProjectorName, &d.EventSeq, &d.EventType,
		&d.ErrorClass, &d.ErrorMessage, &env, &d.CreatedAt, &d.ResolvedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	d.Envelope = json.RawMessage(env)
	return &d, nil
}

// decodeDeadLetterEnvelope reconstructs an eventlog.Envelope from the JSON
// blob the SQLiteDLT writes (see internal/projection/common/dlt.go).
func decodeDeadLetterEnvelope(raw []byte) (eventlog.Envelope, error) {
	var stub struct {
		Seq       int64           `json:"seq"`
		Partition string          `json:"partition"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &stub); err != nil {
		return eventlog.Envelope{}, err
	}
	return eventlog.Envelope{
		Seq:       stub.Seq,
		Partition: stub.Partition,
		Type:      stub.Type,
		Payload:   []byte(stub.Payload),
	}, nil
}

func (s *Server) requireSuperAdmin(w http.ResponseWriter, r *http.Request) bool {
	actor, ok := policy.ActorFrom(r.Context())
	if !ok || !actor.IsSuperAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{
			"code":    "forbidden",
			"message": "super admin required",
		}})
		return false
	}
	return true
}
