package lifecycle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	observationview "github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
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

type CheckpointStore interface {
	Load(context.Context, CheckpointKey) (Checkpoint, bool, error)
	Save(context.Context, Checkpoint) error
}

type WorkspaceCheckpointStore struct {
	ws    workspace.Workspace
	clock func() time.Time
}

func NewWorkspaceCheckpointStore(ws workspace.Workspace) (*WorkspaceCheckpointStore, error) {
	if ws == nil {
		return nil, errors.New("memory lifecycle checkpoint: workspace is required")
	}
	return &WorkspaceCheckpointStore{ws: ws, clock: time.Now}, nil
}

type checkpointEnvelope struct {
	SchemaVersion int        `json:"schema_version"`
	Checkpoint    Checkpoint `json:"checkpoint"`
}

func (store *WorkspaceCheckpointStore) Load(ctx context.Context, key CheckpointKey) (Checkpoint, bool, error) {
	if err := validateCheckpointKey(key); err != nil {
		return Checkpoint{}, false, err
	}
	data, err := store.ws.Read(ctx, store.checkpointPath(key))
	if err != nil {
		if errdefs.IsNotFound(err) {
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

func (store *WorkspaceCheckpointStore) Save(ctx context.Context, checkpoint Checkpoint) error {
	if store == nil || store.ws == nil {
		return errors.New("memory lifecycle checkpoint: store is required")
	}
	if checkpoint.UpdatedAt.IsZero() {
		checkpoint.UpdatedAt = store.clock().UTC()
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	data, err := json.Marshal(checkpointEnvelope{
		SchemaVersion: lifecycleCheckpointSchemaVersion,
		Checkpoint:    checkpoint,
	})
	if err != nil {
		return err
	}
	if err := workspace.AtomicWrite(ctx, store.ws, store.checkpointPath(checkpoint.Key), data); err != nil {
		return fmt.Errorf("memory lifecycle checkpoint: write: %w", err)
	}
	return nil
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

func (store *WorkspaceCheckpointStore) checkpointPath(key CheckpointKey) string {
	return path.Join(
		"checkpoints", "memory-lifecycle", "v1", "partitions",
		encodeCheckpoint(key.Scope.RuntimeID), encodeCheckpoint(key.Scope.UserID), encodeCheckpoint(key.Scope.AgentID),
		"policies", encodeCheckpoint(key.PolicyDigest), "dags", encodeCheckpoint(key.DAGDigest),
		"publications", encodeCheckpoint(key.PublicationID), "branches", encodeCheckpoint(key.Branch),
		encodeCheckpoint(key.Node)+".json",
	)
}

func encodeCheckpoint(value string) string {
	return "k_" + base64.RawURLEncoding.EncodeToString([]byte(value))
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
