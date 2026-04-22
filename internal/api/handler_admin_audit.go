package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	auditcmd "github.com/GizClaw/flowcraft/internal/commands/audit"
	"github.com/GizClaw/flowcraft/internal/policy"
)

type auditEntryDTO struct {
	Seq          int64           `json:"seq"`
	Type         string          `json:"type"`
	ActorID      string          `json:"actor_id,omitempty"`
	ActorKind    string          `json:"actor_kind,omitempty"`
	ActorRealmID string          `json:"actor_realm_id,omitempty"`
	Actor        json.RawMessage `json:"actor,omitempty"`
	Ts           string          `json:"ts"`
	Partition    string          `json:"partition,omitempty"`
	TraceID      string          `json:"trace_id,omitempty"`
	Summary      string          `json:"summary"`
}

type auditListResponse struct {
	Items      []auditEntryDTO `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// handleAdminAuditList answers GET /api/admin/audit. Only super admins may
// call it; the request itself is recorded as an audit_required event before
// the projection completes (D.X "meta-audit").
func (s *Server) handleAdminAuditList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := policy.ActorFrom(ctx)
	if !ok || !actor.IsSuperAdmin() {
		s.metaAuditFailure(ctx, r, auditcmd.ActionAdminViewAudit, "forbidden", "not super admin")
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{
			"code":    "forbidden",
			"message": "super admin required",
		}})
		return
	}
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	cursor := int64(0)
	if v := q.Get("cursor"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cursor = n
		}
	}
	realmFilter := q.Get("realm_id")
	typeFilter := q.Get("type")
	actorFilter := q.Get("actor_id")
	since := q.Get("since")

	rows, err := s.queryAuditEntries(ctx, cursor, limit+1, realmFilter, typeFilter, actorFilter, since)
	if err != nil {
		s.metaAuditFailure(ctx, r, auditcmd.ActionAdminViewAudit, "internal", err.Error())
		writeError(w, err)
		return
	}
	resp := auditListResponse{Items: rows}
	if len(rows) > limit {
		resp.Items = rows[:limit]
		resp.NextCursor = strconv.FormatInt(rows[limit-1].Seq, 10)
	}
	s.metaAuditSuccess(ctx, r, auditcmd.ActionAdminViewAudit)
	writeJSON(w, http.StatusOK, resp)
}

// handleAdminAuditGet answers GET /api/admin/audit/{seq}.
func (s *Server) handleAdminAuditGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := policy.ActorFrom(ctx)
	if !ok || !actor.IsSuperAdmin() {
		s.metaAuditFailure(ctx, r, auditcmd.ActionAdminViewAudit, "forbidden", "not super admin")
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{
			"code":    "forbidden",
			"message": "super admin required",
		}})
		return
	}
	seqStr := r.PathValue("seq")
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{
			"code":    "bad_request",
			"message": "invalid seq",
		}})
		return
	}
	entry, err := s.fetchAuditEntry(ctx, seq)
	if err != nil {
		s.metaAuditFailure(ctx, r, auditcmd.ActionAdminViewAudit, "internal", err.Error())
		writeError(w, err)
		return
	}
	if entry == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{
			"code":    "not_found",
			"message": "audit entry not found",
		}})
		return
	}
	s.metaAuditSuccess(ctx, r, auditcmd.ActionAdminViewAudit)
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) queryAuditEntries(ctx context.Context, cursor int64, limit int, realmFilter, typeFilter, actorFilter, since string) ([]auditEntryDTO, error) {
	db := s.deps.EventLog.DB()
	q := `SELECT seq, type, COALESCE(actor_id,''), COALESCE(actor_kind,''), COALESCE(actor_realm_id,''),
	             COALESCE(actor_json,''), ts, COALESCE(partition,''), COALESCE(trace_id,''), summary
	      FROM audit_entries WHERE 1=1`
	args := []any{}
	if cursor > 0 {
		q += ` AND seq < ?`
		args = append(args, cursor)
	}
	if realmFilter != "" {
		q += ` AND actor_realm_id = ?`
		args = append(args, realmFilter)
	}
	if typeFilter != "" {
		q += ` AND type = ?`
		args = append(args, typeFilter)
	}
	if actorFilter != "" {
		q += ` AND actor_id = ?`
		args = append(args, actorFilter)
	}
	if since != "" {
		q += ` AND ts >= ?`
		args = append(args, since)
	}
	q += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auditEntryDTO
	for rows.Next() {
		var e auditEntryDTO
		var actorJSON string
		if err := rows.Scan(&e.Seq, &e.Type, &e.ActorID, &e.ActorKind, &e.ActorRealmID,
			&actorJSON, &e.Ts, &e.Partition, &e.TraceID, &e.Summary); err != nil {
			return nil, err
		}
		if actorJSON != "" {
			e.Actor = json.RawMessage(actorJSON)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Server) fetchAuditEntry(ctx context.Context, seq int64) (*auditEntryDTO, error) {
	rows, err := s.queryAuditEntries(ctx, seq+1, 1, "", "", "", "")
	if err != nil {
		return nil, err
	}
	for _, e := range rows {
		if e.Seq == seq {
			return &e, nil
		}
	}
	return nil, nil
}

func (s *Server) metaAuditSuccess(ctx context.Context, r *http.Request, action auditcmd.Action) {
	if s.deps.AuditCmds == nil {
		return
	}
	_ = s.deps.AuditCmds.Performed(ctx, auditcmd.PerformedReq{
		Action:     action,
		TargetType: "audit_log",
		TargetID:   r.URL.Path,
		IPAddress:  clientIP(r),
		UserAgent:  r.UserAgent(),
		Details: map[string]any{
			"query":  r.URL.RawQuery,
			"method": r.Method,
		},
	})
}

func (s *Server) metaAuditFailure(ctx context.Context, r *http.Request, action auditcmd.Action, errClass, errMsg string) {
	if s.deps.AuditCmds == nil {
		return
	}
	_ = s.deps.AuditCmds.Failed(ctx, auditcmd.FailedReq{
		Action:       action,
		ErrorClass:   errClass,
		ErrorMessage: errMsg,
		IPAddress:    clientIP(r),
		UserAgent:    r.UserAgent(),
	})
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}

// auditTimeNow is overridable for tests.
var auditTimeNow = func() time.Time { return time.Now().UTC() }

var _ = auditTimeNow
