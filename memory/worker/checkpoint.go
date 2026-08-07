// Package worker derives durable views by scanning canonical memory sources.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/storage"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

const checkpointSchemaVersion = 2

type CheckpointStatus string

const (
	StatusComplete CheckpointStatus = "complete"
	StatusFailed   CheckpointStatus = "failed"
	StatusBlocked  CheckpointStatus = "blocked"
)

type WorkIdentity struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	PolicyDigest string `json:"policy_digest"`
}

type Checkpoint struct {
	Scope     sdkmemory.Scope
	Work      WorkIdentity
	Branch    string
	Status    CheckpointStatus
	Attempt   int
	Error     string
	RunResult *derive.RunResult
	UpdatedAt time.Time
}

// CheckpointStore is the consumer-side contract used by the Processor.
type CheckpointStore interface {
	Load(context.Context, sdkmemory.Scope, WorkIdentity, string) (Checkpoint, bool, error)
	Save(context.Context, Checkpoint) error
	LoadWatermark(context.Context, sdkmemory.Scope, string, string, string) (SourceWatermark, bool, error)
	SaveWatermark(context.Context, SourceWatermark) error
}

// SourceWatermark is the durable, policy-scoped cursor for one canonical
// source stream. Cursor advances only after every branch for the source item
// has completed.
type SourceWatermark struct {
	Scope        sdkmemory.Scope `json:"scope"`
	StreamKind   string          `json:"stream_kind"`
	StreamID     string          `json:"stream_id"`
	PolicyDigest string          `json:"policy_digest"`
	Cursor       uint64          `json:"cursor"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// WorkerCheckpointStore persists derivation checkpoints and source
// watermarks on a storage.Store.
type WorkerCheckpointStore struct {
	kv    storage.Store
	clock func() time.Time
}

type persistedCheckpoint struct {
	SchemaVersion int               `json:"schema_version"`
	Scope         sdkmemory.Scope   `json:"scope"`
	Work          WorkIdentity      `json:"work"`
	Branch        string            `json:"branch"`
	Status        CheckpointStatus  `json:"status"`
	Attempt       int               `json:"attempt"`
	Error         string            `json:"error,omitempty"`
	RunResult     *derive.RunResult `json:"run_result,omitempty"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

type persistedWatermark struct {
	SchemaVersion int             `json:"schema_version"`
	Watermark     SourceWatermark `json:"watermark"`
}

// NewWorkerCheckpointStore constructs a KV-backed derivation checkpoint
// store.
func NewWorkerCheckpointStore(kv storage.Store) (*WorkerCheckpointStore, error) {
	if nilInterface(kv) {
		return nil, errors.New("memory worker checkpoint: store is required")
	}
	return &WorkerCheckpointStore{kv: kv, clock: time.Now}, nil
}

// Load implements CheckpointStore.
func (store *WorkerCheckpointStore) Load(ctx context.Context, scope sdkmemory.Scope, work WorkIdentity, branch string) (Checkpoint, bool, error) {
	if err := validateKey(scope, work, branch); err != nil {
		return Checkpoint{}, false, err
	}
	key, err := store.checkpointKey(scope, work, branch)
	if err != nil {
		return Checkpoint{}, false, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Checkpoint{}, false, nil
		}
		return Checkpoint{}, false, fmt.Errorf("memory worker checkpoint: read: %w", err)
	}
	var value persistedCheckpoint
	if err := decodeStrict(data, &value); err != nil {
		return Checkpoint{}, false, fmt.Errorf("memory worker checkpoint: decode: %w", err)
	}
	if value.SchemaVersion != checkpointSchemaVersion {
		return Checkpoint{}, false, fmt.Errorf("memory worker checkpoint: unsupported schema_version %d", value.SchemaVersion)
	}
	if value.Scope != scope || value.Work != work || value.Branch != branch {
		return Checkpoint{}, false, errors.New("memory worker checkpoint: persisted address does not match key")
	}
	checkpoint := Checkpoint{
		Scope: value.Scope, Work: value.Work, Branch: value.Branch, Status: value.Status,
		Attempt: value.Attempt, Error: value.Error, RunResult: cloneRunResult(value.RunResult), UpdatedAt: value.UpdatedAt,
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return Checkpoint{}, false, fmt.Errorf("memory worker checkpoint: corrupt: %w", err)
	}
	return checkpoint, true, nil
}

// Save implements CheckpointStore.
func (store *WorkerCheckpointStore) Save(ctx context.Context, checkpoint Checkpoint) error {
	if store == nil || nilInterface(store.kv) {
		return errors.New("memory worker checkpoint: store is required")
	}
	checkpoint.RunResult = cloneRunResult(checkpoint.RunResult)
	if checkpoint.UpdatedAt.IsZero() {
		checkpoint.UpdatedAt = store.clock()
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	key, err := store.checkpointKey(checkpoint.Scope, checkpoint.Work, checkpoint.Branch)
	if err != nil {
		return err
	}
	data, err := json.Marshal(persistedCheckpoint{
		SchemaVersion: checkpointSchemaVersion, Scope: checkpoint.Scope, Work: checkpoint.Work,
		Branch: checkpoint.Branch, Status: checkpoint.Status, Attempt: checkpoint.Attempt,
		Error: checkpoint.Error, RunResult: checkpoint.RunResult, UpdatedAt: checkpoint.UpdatedAt,
	})
	if err != nil {
		return fmt.Errorf("memory worker checkpoint: encode: %w", err)
	}
	if err := store.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("memory worker checkpoint: write: %w", err)
	}
	return nil
}

// LoadWatermark implements CheckpointStore.
func (store *WorkerCheckpointStore) LoadWatermark(
	ctx context.Context,
	scope sdkmemory.Scope,
	streamKind string,
	streamID string,
	policyDigest string,
) (SourceWatermark, bool, error) {
	key := SourceWatermark{
		Scope: scope, StreamKind: streamKind, StreamID: streamID, PolicyDigest: policyDigest,
	}
	if err := validateWatermarkKey(key); err != nil {
		return SourceWatermark{}, false, err
	}
	watermarkKey, err := store.watermarkKey(key)
	if err != nil {
		return SourceWatermark{}, false, err
	}
	data, err := store.kv.Get(ctx, watermarkKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return SourceWatermark{}, false, nil
		}
		return SourceWatermark{}, false, fmt.Errorf("memory worker watermark: read: %w", err)
	}
	var persisted persistedWatermark
	if err := decodeStrict(data, &persisted); err != nil {
		return SourceWatermark{}, false, fmt.Errorf("memory worker watermark: decode: %w", err)
	}
	if persisted.SchemaVersion != checkpointSchemaVersion {
		return SourceWatermark{}, false, fmt.Errorf("memory worker watermark: unsupported schema_version %d", persisted.SchemaVersion)
	}
	value := persisted.Watermark
	if value.Scope != scope || value.StreamKind != streamKind || value.StreamID != streamID ||
		value.PolicyDigest != policyDigest || value.UpdatedAt.IsZero() {
		return SourceWatermark{}, false, errors.New("memory worker watermark: persisted address does not match key")
	}
	return value, true, nil
}

// SaveWatermark implements CheckpointStore.
func (store *WorkerCheckpointStore) SaveWatermark(ctx context.Context, value SourceWatermark) error {
	if store == nil || nilInterface(store.kv) {
		return errors.New("memory worker watermark: store is required")
	}
	if err := validateWatermarkKey(value); err != nil {
		return err
	}
	if value.UpdatedAt.IsZero() {
		value.UpdatedAt = store.clock()
	}
	current, ok, err := store.LoadWatermark(
		ctx, value.Scope, value.StreamKind, value.StreamID, value.PolicyDigest,
	)
	if err != nil {
		return err
	}
	if ok && value.Cursor < current.Cursor {
		return errors.New("memory worker watermark: cursor must not move backwards")
	}
	key, err := store.watermarkKey(value)
	if err != nil {
		return err
	}
	data, err := json.Marshal(persistedWatermark{
		SchemaVersion: checkpointSchemaVersion, Watermark: value,
	})
	if err != nil {
		return fmt.Errorf("memory worker watermark: encode: %w", err)
	}
	if err := store.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("memory worker watermark: write: %w", err)
	}
	return nil
}

func (store *WorkerCheckpointStore) checkpointKey(scope sdkmemory.Scope, work WorkIdentity, branch string) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return "worker/v1/checkpoints/" + partition + "/" +
		storage.EncodeSegment(work.PolicyDigest) + "/" +
		storage.EncodeSegment(work.Kind) + "/" +
		storage.EncodeSegment(work.ID) + "/" +
		storage.EncodeSegment(branch), nil
}

func (store *WorkerCheckpointStore) watermarkKey(value SourceWatermark) (string, error) {
	partition, err := storage.ScopePartition(value.Scope)
	if err != nil {
		return "", err
	}
	return "worker/v1/watermarks/" + partition + "/" +
		storage.EncodeSegment(value.PolicyDigest) + "/" +
		storage.EncodeSegment(value.StreamKind) + "/" +
		storage.EncodeSegment(value.StreamID), nil
}

func validateWatermarkKey(value SourceWatermark) error {
	if err := value.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(value.StreamKind) == "" || strings.TrimSpace(value.StreamID) == "" ||
		strings.TrimSpace(value.PolicyDigest) == "" {
		return errors.New("memory worker watermark: stream kind, stream id, and policy digest are required")
	}
	return nil
}

func validateKey(scope sdkmemory.Scope, work WorkIdentity, branch string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(work.Kind) == "" || strings.TrimSpace(work.ID) == "" ||
		strings.TrimSpace(work.PolicyDigest) == "" || strings.TrimSpace(branch) == "" {
		return errors.New("memory worker checkpoint: work kind, id, policy digest, and branch are required")
	}
	return nil
}

func validateCheckpoint(value Checkpoint) error {
	if err := validateKey(value.Scope, value.Work, value.Branch); err != nil {
		return err
	}
	if value.Attempt <= 0 || value.UpdatedAt.IsZero() {
		return errors.New("memory worker checkpoint: attempt and updated_at are required")
	}
	switch value.Status {
	case StatusComplete:
		if value.Error != "" {
			return errors.New("memory worker checkpoint: complete checkpoint has error")
		}
	case StatusFailed, StatusBlocked:
		if value.Error == "" {
			return errors.New("memory worker checkpoint: unsuccessful checkpoint requires error")
		}
	default:
		return fmt.Errorf("memory worker checkpoint: invalid status %q", value.Status)
	}
	return nil
}

func cloneRunResult(value *derive.RunResult) *derive.RunResult {
	if value == nil {
		return nil
	}
	cloned := value.Clone()
	return &cloned
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
