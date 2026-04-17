package version

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/rs/xid"
)

// DataStore is the subset of model.Store used by VersionStore for persistence.
type DataStore interface {
	ListGraphVersions(ctx context.Context, agentID string) ([]*model.GraphVersion, error)
	GetGraphVersion(ctx context.Context, agentID string, version int) (*model.GraphVersion, error)
	SaveGraphVersion(ctx context.Context, gv *model.GraphVersion) error
	PublishGraphVersion(ctx context.Context, agentID string, def *model.GraphDefinition, description string) (*model.GraphVersion, error)
	GetLatestPublishedVersion(ctx context.Context, agentID string) (*model.GraphVersion, error)
	UpdateVersionLock(ctx context.Context, agentID string, expectedChecksum string, newDef *model.GraphDefinition) error
}

// VersionStore provides high-level version management (draft/publish/rollback/diff).
type VersionStore interface {
	SaveDraft(ctx context.Context, gv *model.GraphVersion) error
	Publish(ctx context.Context, agentID string, version int, description string) (*model.GraphVersion, error)
	Rollback(ctx context.Context, agentID string, toVersion int) (*model.GraphVersion, error)
	Diff(ctx context.Context, agentID string, v1, v2 int) (*GraphDiff, error)
	ListVersions(ctx context.Context, agentID string) ([]*model.GraphVersion, error)
	GetPublished(ctx context.Context, agentID string) (*model.GraphVersion, error)
	GetDraft(ctx context.Context, agentID string) (*model.GraphVersion, error)
}

// versionStore provides version management backed by DataStore. Thread-safe.
type versionStore struct {
	store DataStore
}

// NewVersionStore creates a VersionStore backed by the given DataStore.
func NewVersionStore(s DataStore) VersionStore {
	return &versionStore{store: s}
}

func (vs *versionStore) SaveDraft(ctx context.Context, gv *model.GraphVersion) error {
	gv.Checksum = ComputeChecksum(gv.GraphDef)

	if gv.ID == "" {
		gv.ID = xid.New().String()
	}
	if gv.CreatedAt.IsZero() {
		gv.CreatedAt = time.Now()
	}
	// Draft: PublishedAt stays nil.
	gv.PublishedAt = nil

	// Optimistic lock: if a BaseChecksum is provided via Checksum comparison,
	// the caller should use UpdateVersionLock directly for CAS semantics.
	return vs.store.SaveGraphVersion(ctx, gv)
}

func (vs *versionStore) Publish(ctx context.Context, agentID string, version int, description string) (*model.GraphVersion, error) {
	if version <= 0 {
		draft, err := vs.GetDraft(ctx, agentID)
		if err != nil {
			return nil, err
		}
		version = draft.Version
	}
	target, err := vs.store.GetGraphVersion(ctx, agentID, version)
	if err != nil {
		return nil, err
	}

	if target.PublishedAt != nil {
		return nil, errdefs.Conflictf("version %d is already published", version)
	}

	latest, latestErr := vs.store.GetLatestPublishedVersion(ctx, agentID)
	if latestErr == nil && latest.GraphDef != nil {
		if ComputeChecksum(target.GraphDef) == ComputeChecksum(latest.GraphDef) {
			return nil, errdefs.Conflictf("no changes since v%d", latest.Version)
		}
	}

	now := time.Now()
	target.PublishedAt = &now
	if description != "" {
		target.Description = description
	}
	target.CreatedBy = "publish"

	if err := vs.store.SaveGraphVersion(ctx, target); err != nil {
		return nil, err
	}

	nextDraft := &model.GraphVersion{
		AgentID:  agentID,
		Version:  target.Version + 1,
		GraphDef: target.GraphDef,
	}
	_ = vs.SaveDraft(ctx, nextDraft)

	return target, nil
}

func (vs *versionStore) Rollback(ctx context.Context, agentID string, toVersion int) (*model.GraphVersion, error) {
	target, err := vs.store.GetGraphVersion(ctx, agentID, toVersion)
	if err != nil {
		return nil, err
	}

	gv, err := vs.store.PublishGraphVersion(ctx, agentID, target.GraphDef, fmt.Sprintf("rollback to v%d", toVersion))
	if err != nil {
		return nil, err
	}
	gv.CreatedBy = "rollback"
	_ = vs.store.SaveGraphVersion(ctx, gv)

	nextDraft := &model.GraphVersion{
		AgentID:  agentID,
		Version:  gv.Version + 1,
		GraphDef: gv.GraphDef,
	}
	_ = vs.SaveDraft(ctx, nextDraft)

	return gv, nil
}

func (vs *versionStore) Diff(ctx context.Context, agentID string, v1, v2 int) (*GraphDiff, error) {
	ver1, err := vs.store.GetGraphVersion(ctx, agentID, v1)
	if err != nil {
		return nil, err
	}
	ver2, err := vs.store.GetGraphVersion(ctx, agentID, v2)
	if err != nil {
		return nil, err
	}
	return computeDiff(ver1.GraphDef, ver2.GraphDef, v1, v2), nil
}

func (vs *versionStore) ListVersions(ctx context.Context, agentID string) ([]*model.GraphVersion, error) {
	return vs.store.ListGraphVersions(ctx, agentID)
}

func (vs *versionStore) GetPublished(ctx context.Context, agentID string) (*model.GraphVersion, error) {
	return vs.store.GetLatestPublishedVersion(ctx, agentID)
}

func (vs *versionStore) GetDraft(ctx context.Context, agentID string) (*model.GraphVersion, error) {
	versions, err := vs.store.ListGraphVersions(ctx, agentID)
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		if v.PublishedAt == nil {
			return v, nil
		}
	}
	return nil, errdefs.NotFoundf("draft %s", agentID)
}
