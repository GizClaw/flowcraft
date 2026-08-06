package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const repairAuditSchemaVersion = 1

type repairPlanEnvelope struct {
	SchemaVersion int        `json:"schema_version"`
	Plan          RepairPlan `json:"plan"`
}

// WorkspaceRepairAuditStore persists immutable repair evidence and actions.
type WorkspaceRepairAuditStore struct {
	ws workspace.Workspace
}

func NewWorkspaceRepairAuditStore(ws workspace.Workspace) (*WorkspaceRepairAuditStore, error) {
	if ws == nil {
		return nil, errors.New("memory lifecycle repair audit: workspace is required")
	}
	return &WorkspaceRepairAuditStore{ws: ws}, nil
}

func (store *WorkspaceRepairAuditStore) Save(ctx context.Context, plan RepairPlan) error {
	if store == nil || store.ws == nil {
		return errors.New("memory lifecycle repair audit: store is required")
	}
	if err := plan.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(plan.ID) == "" || plan.AlgorithmVersion != RepairAlgorithmVersion {
		return errors.New("memory lifecycle repair audit: plan identity and version are required")
	}
	data, err := json.Marshal(repairPlanEnvelope{SchemaVersion: repairAuditSchemaVersion, Plan: plan})
	if err != nil {
		return err
	}
	target := store.planPath(plan.Scope, plan.ID)
	existing, err := store.ws.Read(ctx, target)
	if err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return errdefs.Conflictf("memory lifecycle repair audit: immutable plan %q conflicts", plan.ID)
	}
	if !errdefs.IsNotFound(err) {
		return err
	}
	if err := workspace.AtomicWrite(ctx, store.ws, target, data); err != nil {
		return fmt.Errorf("memory lifecycle repair audit: save plan: %w", err)
	}
	return nil
}

func (store *WorkspaceRepairAuditStore) planPath(scope sdkmemory.Scope, id string) string {
	return path.Join("audit", "memory-lifecycle", "repair", "v1", "partitions",
		encodeLifecycle(scope.RuntimeID), encodeLifecycle(scope.UserID), encodeLifecycle(scope.AgentID),
		encodeLifecycle(id)+".json")
}
