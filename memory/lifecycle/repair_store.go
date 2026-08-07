package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/memory/storage"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const repairEventType = "repair.plan"

// RepairAuditStore persists immutable repair evidence and actions as events
// in a storage.Log.
type RepairAuditStore struct {
	log storage.Log
}

// NewRepairAuditStore constructs a Log-backed repair audit store.
func NewRepairAuditStore(log storage.Log) (*RepairAuditStore, error) {
	if nilStore(log) {
		return nil, errors.New("memory lifecycle repair audit: log is required")
	}
	return &RepairAuditStore{log: log}, nil
}

// Save appends one immutable repair plan. Retrying the same plan ID with the
// same content is idempotent; different content under the same ID conflicts.
func (store *RepairAuditStore) Save(ctx context.Context, plan RepairPlan) error {
	if store == nil || nilStore(store.log) {
		return errors.New("memory lifecycle repair audit: store is required")
	}
	if err := plan.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(plan.ID) == "" || plan.AlgorithmVersion != RepairAlgorithmVersion {
		return errors.New("memory lifecycle repair audit: plan identity and version are required")
	}
	partition, err := storage.ScopePartition(plan.Scope)
	if err != nil {
		return err
	}
	stream := "audit/v1/repair/" + partition
	payload, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("memory lifecycle repair audit: encode plan: %w", err)
	}
	_, err = store.log.Append(ctx, stream, []storage.Event{{
		Stream:  stream,
		Type:    repairEventType,
		Payload: payload,
	}}, storage.AppendOptions{IdempotencyKey: plan.ID})
	if err != nil {
		if errors.Is(err, storage.ErrConflict) {
			return errdefs.Conflictf("memory lifecycle repair audit: immutable plan %q conflicts", plan.ID)
		}
		return fmt.Errorf("memory lifecycle repair audit: save plan: %w", err)
	}
	return nil
}
