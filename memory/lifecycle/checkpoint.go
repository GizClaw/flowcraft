package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/storage"
	observationview "github.com/GizClaw/flowcraft/memory/views/observation"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

const lifecycleCheckpointSchemaVersion = 1

type CheckpointStatus string

const (
	CheckpointCompleted CheckpointStatus = "completed"
	CheckpointError     CheckpointStatus = "error"
)

type CheckpointKey struct {
	Scope         sdkmemory.Scope `json:"scope"`
	PublicationID string          `json:"publication_id"`
	Node          string          `json:"node"`
	Branch        string          `json:"branch"`
	PolicyDigest  string          `json:"policy_digest"`
	DAGDigest     string          `json:"dag_digest"`
}

type Checkpoint struct {
	Key       CheckpointKey    `json:"key"`
	Status    CheckpointStatus `json:"status"`
	Error     string           `json:"error,omitempty"`
	State     *CheckpointState `json:"state,omitempty"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// CheckpointState contains only typed, replay-safe node outputs.
type CheckpointState struct {
	Observation  *observationview.Observation  `json:"observation,omitempty"`
	Observations []observationview.Observation `json:"observations,omitempty"`
	Scores       []ScoreSnapshot               `json:"scores,omitempty"`
	ForgetPlan   *ForgetPlan                   `json:"forget_plan,omitempty"`
	RepairPlan   *RepairPlan                   `json:"repair_plan,omitempty"`
}

// CheckpointStore is the consumer-side contract used by the lifecycle DAG.
type CheckpointStore interface {
	Load(context.Context, CheckpointKey) (Checkpoint, bool, error)
	Save(context.Context, Checkpoint) error
}

// LifecycleCheckpointStore persists lifecycle DAG node checkpoints on a
// storage.Store.
type LifecycleCheckpointStore struct {
	kv    storage.Store
	clock func() time.Time
}

type checkpointEnvelope struct {
	SchemaVersion int        `json:"schema_version"`
	Checkpoint    Checkpoint `json:"checkpoint"`
}

// NewLifecycleCheckpointStore constructs a KV-backed lifecycle checkpoint
// store.
func NewLifecycleCheckpointStore(kv storage.Store) (*LifecycleCheckpointStore, error) {
	if nilStore(kv) {
		return nil, errors.New("memory lifecycle checkpoint: store is required")
	}
	return &LifecycleCheckpointStore{kv: kv, clock: time.Now}, nil
}

// Load implements CheckpointStore.
func (store *LifecycleCheckpointStore) Load(ctx context.Context, key CheckpointKey) (Checkpoint, bool, error) {
	if err := validateCheckpointKey(key); err != nil {
		return Checkpoint{}, false, err
	}
	checkpointKey, err := store.key(key)
	if err != nil {
		return Checkpoint{}, false, err
	}
	data, err := store.kv.Get(ctx, checkpointKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Checkpoint{}, false, nil
		}
		return Checkpoint{}, false, err
	}
	var envelope checkpointEnvelope
	if err := strictOutboxDecode(data, &envelope); err != nil {
		return Checkpoint{}, false, fmt.Errorf("memory lifecycle checkpoint: decode: %w", err)
	}
	if envelope.SchemaVersion != lifecycleCheckpointSchemaVersion || envelope.Checkpoint.Key != key {
		return Checkpoint{}, false, errors.New("memory lifecycle checkpoint: persisted address mismatch")
	}
	if err := validateCheckpoint(envelope.Checkpoint); err != nil {
		return Checkpoint{}, false, err
	}
	return envelope.Checkpoint, true, nil
}

// Save implements CheckpointStore.
func (store *LifecycleCheckpointStore) Save(ctx context.Context, checkpoint Checkpoint) error {
	if store == nil || nilStore(store.kv) {
		return errors.New("memory lifecycle checkpoint: store is required")
	}
	if checkpoint.UpdatedAt.IsZero() {
		checkpoint.UpdatedAt = store.clock().UTC()
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	key, err := store.key(checkpoint.Key)
	if err != nil {
		return err
	}
	data, err := json.Marshal(checkpointEnvelope{
		SchemaVersion: lifecycleCheckpointSchemaVersion,
		Checkpoint:    checkpoint,
	})
	if err != nil {
		return err
	}
	if err := store.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("memory lifecycle checkpoint: write: %w", err)
	}
	return nil
}

func (store *LifecycleCheckpointStore) key(key CheckpointKey) (string, error) {
	partition, err := storage.ScopePartition(key.Scope)
	if err != nil {
		return "", err
	}
	return "lifecycle/v1/checkpoints/" + partition + "/" +
		storage.EncodeSegment(key.PolicyDigest) + "/" +
		storage.EncodeSegment(key.DAGDigest) + "/" +
		storage.EncodeSegment(key.PublicationID) + "/" +
		storage.EncodeSegment(key.Branch) + "/" +
		storage.EncodeSegment(key.Node), nil
}

func validateCheckpoint(value Checkpoint) error {
	if err := validateCheckpointKey(value.Key); err != nil {
		return err
	}
	if value.UpdatedAt.IsZero() {
		return errors.New("memory lifecycle checkpoint: updated_at is required")
	}
	switch value.Status {
	case CheckpointCompleted:
		if value.Error != "" {
			return errors.New("memory lifecycle checkpoint: completed record has error")
		}
	case CheckpointError:
		if strings.TrimSpace(value.Error) == "" {
			return errors.New("memory lifecycle checkpoint: error record requires message")
		}
	default:
		return fmt.Errorf("memory lifecycle checkpoint: invalid status %q", value.Status)
	}
	return nil
}

func validateCheckpointKey(key CheckpointKey) error {
	if err := key.Scope.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"publication": key.PublicationID, "node": key.Node, "branch": key.Branch,
		"policy": key.PolicyDigest, "DAG": key.DAGDigest,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("memory lifecycle checkpoint: %s is required", name)
		}
	}
	return nil
}

type memoryCheckpointStore struct {
	mu     sync.Mutex
	values map[CheckpointKey]Checkpoint
}

func newMemoryCheckpointStore() *memoryCheckpointStore {
	return &memoryCheckpointStore{values: make(map[CheckpointKey]Checkpoint)}
}

func (store *memoryCheckpointStore) Load(_ context.Context, key CheckpointKey) (Checkpoint, bool, error) {
	if err := validateCheckpointKey(key); err != nil {
		return Checkpoint{}, false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	value, ok := store.values[key]
	return cloneCheckpoint(value), ok, nil
}

func (store *memoryCheckpointStore) Save(_ context.Context, checkpoint Checkpoint) error {
	if checkpoint.UpdatedAt.IsZero() {
		checkpoint.UpdatedAt = time.Now().UTC()
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	store.mu.Lock()
	store.values[checkpoint.Key] = cloneCheckpoint(checkpoint)
	store.mu.Unlock()
	return nil
}

func cloneCheckpoint(value Checkpoint) Checkpoint {
	if value.State == nil {
		return value
	}
	data, err := json.Marshal(value.State)
	if err != nil {
		return value
	}
	var state CheckpointState
	if err := json.Unmarshal(data, &state); err != nil {
		return value
	}
	value.State = &state
	return value
}

func nilStore(value any) bool {
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
