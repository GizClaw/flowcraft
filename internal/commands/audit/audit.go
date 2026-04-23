// Package audit contains command handlers for the explicit audit.action.*
// envelopes. Business code calls Performed / Failed when an action is not
// already represented by a domain envelope (e.g. login, viewing audit logs,
// permission denial).
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// Action is the well-known set of admin / auth actions worth auditing.
type Action string

const (
	ActionLogin            Action = "login"
	ActionLogout           Action = "logout"
	ActionAdminViewAudit   Action = "admin_view_audit"
	ActionAdminViewDLT     Action = "admin_view_dlt"
	ActionAdminReplay      Action = "admin_replay"
	ActionPermissionDenied Action = "permission_denied"
	ActionRateLimited      Action = "rate_limited"
	ActionMfaFailed        Action = "mfa_failed"
)

// PerformedReq describes a successful audit action.
type PerformedReq struct {
	Action     Action
	TargetType string
	TargetID   string
	IPAddress  string
	UserAgent  string
	Details    map[string]any
	CommandID  string
}

// FailedReq describes a failed audit action.
type FailedReq struct {
	Action       Action
	ErrorClass   string // auth_failure / forbidden / rate_limit / validation
	ErrorMessage string
	IPAddress    string
	UserAgent    string
	CommandID    string
}

// Commands publishes explicit audit events.
type Commands struct {
	log eventlog.Log
}

// New constructs the bundle.
func New(log eventlog.Log) *Commands {
	return &Commands{log: log}
}

// Performed publishes audit.action.performed for the actor in ctx.
func (c *Commands) Performed(ctx context.Context, req PerformedReq) error {
	actor, _ := policy.ActorFrom(ctx)
	if actor.ID == "" {
		actor.ID = "anonymous"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := c.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishAuditActionPerformedInTx(ctx, uow, actor.ID, eventlog.AuditActionPerformedPayload{
			ActorID:    actor.ID,
			Action:     string(req.Action),
			TargetType: req.TargetType,
			TargetID:   req.TargetID,
			IPAddress:  req.IPAddress,
			UserAgent:  req.UserAgent,
			OccurredAt: now,
			Details:    req.Details,
		}, eventlog.WithActor(actor.ToWire()))
	})
	return err
}

// Failed publishes audit.action.failed for the actor in ctx.
func (c *Commands) Failed(ctx context.Context, req FailedReq) error {
	actor, _ := policy.ActorFrom(ctx)
	if actor.ID == "" {
		actor.ID = "anonymous"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := c.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishAuditActionFailedInTx(ctx, uow, actor.ID, eventlog.AuditActionFailedPayload{
			ActorID:      actor.ID,
			Action:       string(req.Action),
			ErrorClass:   req.ErrorClass,
			ErrorMessage: req.ErrorMessage,
			IPAddress:    req.IPAddress,
			UserAgent:    req.UserAgent,
			OccurredAt:   now,
		}, eventlog.WithActor(actor.ToWire()))
	})
	return err
}


// newCommandID is exported via this private symbol so tests can stub via build tags if needed.
func newCommandID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var _ = newCommandID // not yet used; reserved for dedup wiring in handlers
