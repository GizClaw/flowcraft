// Package realm contains command handlers for realm.* events. Realm-level
// state changes (creation, config edits, membership churn) are emitted as
// audit-required envelopes so they show up in admin audit views.
package realm

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// CreateReq creates a new realm row.
type CreateReq struct {
	RealmID   string
	Name      string
	CommandID string
}

// ConfigUpdateReq mutates a subset of realm settings.
type ConfigUpdateReq struct {
	RealmID   string
	Keys      []string
	CommandID string
}

// MemberAddReq grants membership to a user.
type MemberAddReq struct {
	RealmID   string
	MemberID  string
	Role      string
	CommandID string
}

// MemberRemoveReq revokes membership from a user.
type MemberRemoveReq struct {
	RealmID   string
	MemberID  string
	CommandID string
}

// Commands bundles the four realm commands so callers wire them once.
type Commands struct {
	log eventlog.Log
}

// New constructs the bundle.
func New(log eventlog.Log) *Commands {
	return &Commands{log: log}
}

// Create publishes realm.created.
func (c *Commands) Create(ctx context.Context, req CreateReq) error {
	if req.CommandID == "" {
		req.CommandID = newCommandID()
	}
	actor, _ := policy.ActorFrom(ctx)
	return c.atomicWithDedup(ctx, req.CommandID, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishRealmCreatedInTx(ctx, uow, req.RealmID, eventlog.RealmCreatedPayload{
			RealmID: req.RealmID,
			Name:    req.Name,
		}, eventlog.WithActor(actor.ToWire()))
	})
}

// ConfigUpdate publishes realm.config.changed with the list of keys touched.
func (c *Commands) ConfigUpdate(ctx context.Context, req ConfigUpdateReq) error {
	if req.CommandID == "" {
		req.CommandID = newCommandID()
	}
	keys := append([]string(nil), req.Keys...)
	sort.Strings(keys)
	actor, _ := policy.ActorFrom(ctx)
	return c.atomicWithDedup(ctx, req.CommandID, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishRealmConfigChangedInTx(ctx, uow, req.RealmID, eventlog.RealmConfigChangedPayload{
			RealmID: req.RealmID,
			Keys:    keys,
		}, eventlog.WithActor(actor.ToWire()))
	})
}

// MemberAdd publishes realm.member.added.
func (c *Commands) MemberAdd(ctx context.Context, req MemberAddReq) error {
	if req.CommandID == "" {
		req.CommandID = newCommandID()
	}
	if req.Role == "" {
		req.Role = "member"
	}
	actor, _ := policy.ActorFrom(ctx)
	return c.atomicWithDedup(ctx, req.CommandID, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishRealmMemberAddedInTx(ctx, uow, req.RealmID, eventlog.RealmMemberAddedPayload{
			RealmID:  req.RealmID,
			MemberID: req.MemberID,
			Role:     req.Role,
		}, eventlog.WithActor(actor.ToWire()))
	})
}

// MemberRemove publishes realm.member.removed.
func (c *Commands) MemberRemove(ctx context.Context, req MemberRemoveReq) error {
	if req.CommandID == "" {
		req.CommandID = newCommandID()
	}
	actor, _ := policy.ActorFrom(ctx)
	return c.atomicWithDedup(ctx, req.CommandID, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishRealmMemberRemovedInTx(ctx, uow, req.RealmID, eventlog.RealmMemberRemovedPayload{
			RealmID:  req.RealmID,
			MemberID: req.MemberID,
		}, eventlog.WithActor(actor.ToWire()))
	})
}

func (c *Commands) atomicWithDedup(ctx context.Context, commandID string, body func(eventlog.UnitOfWork) error) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := c.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		var dummy int
		row := uow.BusinessQueryRow(ctx, `SELECT 1 FROM command_dedup WHERE command_id=?`, commandID)
		if err := row.Scan(&dummy); err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := uow.BusinessExec(ctx,
			`INSERT INTO command_dedup(command_id, executed_at) VALUES(?, ?)`,
			commandID, now); err != nil {
			return err
		}
		return body(uow)
	})
	return err
}

func newCommandID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

