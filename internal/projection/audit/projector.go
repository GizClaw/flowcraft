// Package audit contains the AuditProjector which writes audit entries for
// all events with audit_required=true.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// ProjectorName is the canonical name for the audit projector.
const ProjectorName = "audit"

// AuditProjector writes audit entries for all audit_required events.
// The subscription list is generated automatically by eventgen from events.yaml.
type AuditProjector struct {
	log eventlog.Log
}

var _ projection.Projector = (*AuditProjector)(nil)

// NewAuditProjector constructs an AuditProjector.
func NewAuditProjector(log eventlog.Log) *AuditProjector {
	return &AuditProjector{log: log}
}

func (p *AuditProjector) Name() string { return ProjectorName }

// Subscribes returns all event types that require auditing (codegen).
func (p *AuditProjector) Subscribes() []string {
	return eventlog.AuditRequiredEventTypes
}

func (p *AuditProjector) RestoreMode() projection.RestoreMode { return projection.RestoreReplay }
func (p *AuditProjector) OnReady(context.Context) error        { return nil }

// AuditProjector does not use snapshots.
func (p *AuditProjector) SnapshotFormatVersion() int                  { return 0 }
func (p *AuditProjector) SnapshotEvery() (int64, time.Duration)     { return 0, 0 }
func (p *AuditProjector) Snapshot(context.Context) (int64, []byte, error) {
	return 0, nil, nil
}
func (p *AuditProjector) LoadSnapshot(context.Context, int64, []byte) error { return nil }

// Apply writes an audit_entries row for each audit_required event. Inserts
// run inside the projector uow so audit is atomic with the checkpoint update;
// duplicate inserts on retry are absorbed by the (seq) primary key.
func (p *AuditProjector) Apply(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var (
		actorID, actorKind, actorRealm string
		actorJSON                      string
	)
	if env.Actor != nil {
		actorID = env.Actor.ID
		actorKind = env.Actor.Kind
		actorRealm = env.Actor.RealmID
		if b, err := json.Marshal(env.Actor); err == nil {
			actorJSON = string(b)
		}
	}
	summary := eventlog.RenderAuditSummary(env)
	if summary == "" {
		summary = fmt.Sprintf("%s event for %s", env.Type, env.Partition)
	}
	if _, err := uow.BusinessExec(ctx, `
		INSERT OR IGNORE INTO audit_entries(seq, type, actor_id, actor_kind, actor_realm_id, actor_json, ts, partition, trace_id, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		env.Seq, env.Type, actorID, actorKind, actorRealm, actorJSON,
		env.Ts, env.Partition, env.TraceID, summary); err != nil {
		slog.Warn("audit projector: insert failed", "seq", env.Seq, "type", env.Type, "err", err)
		return err
	}
	return nil
}
