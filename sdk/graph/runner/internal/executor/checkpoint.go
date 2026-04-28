package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/graph"
)

// Checkpoint represents a snapshot of graph execution state.
//
// Deprecated: use engine.Checkpoint. This type predates the
// engine.Host.Checkpointer abstraction; the executor now persists
// checkpoints through the host. Scheduled for removal in v0.3.0
// together with WithCheckpointStore. Existing CheckpointStore
// implementations keep working via storeOnlyHost.
type Checkpoint struct {
	GraphName string               `json:"graph_name"`
	RunID     string               `json:"run_id,omitempty"`
	NodeID    string               `json:"node_id"`
	Iteration int                  `json:"iteration"`
	Board     *graph.BoardSnapshot `json:"board"`
	Timestamp time.Time            `json:"timestamp"`
}

// toEngine projects the legacy executor.Checkpoint onto the canonical
// engine.Checkpoint shape. NodeID becomes Step (the engine-defined
// position marker); GraphName lands in Attributes so subscribers that
// index by graph still find it.
func (c Checkpoint) toEngine() engine.Checkpoint {
	attrs := map[string]string{}
	if c.GraphName != "" {
		attrs["graph_name"] = c.GraphName
	}
	return engine.Checkpoint{
		ExecID:     c.RunID,
		Step:       c.NodeID,
		Iteration:  c.Iteration,
		Board:      c.Board,
		Attributes: attrs,
		Timestamp:  c.Timestamp,
	}
}

// checkpointFromEngine is the inverse of toEngine. Used by storeOnlyHost
// when the executor (now host-driven) hands a checkpoint back to a
// legacy CheckpointStore.
func checkpointFromEngine(cp engine.Checkpoint) Checkpoint {
	return Checkpoint{
		GraphName: cp.Attributes["graph_name"],
		RunID:     cp.ExecID,
		NodeID:    cp.Step,
		Iteration: cp.Iteration,
		Board:     cp.Board,
		Timestamp: cp.Timestamp,
	}
}

// CheckpointStore is the interface for persisting and loading checkpoints.
//
// Deprecated: implement engine.CheckpointStore (or expose a Host with a
// real Checkpointer) and pass it via WithHost. The executor now writes
// every checkpoint through host.Checkpoint; this interface is kept so
// code already using WithCheckpointStore keeps compiling and is folded
// into the host path via storeOnlyHost. Scheduled for removal in v0.3.0.
type CheckpointStore interface {
	Save(cp Checkpoint) error
	// Load retrieves the latest checkpoint. When runID is non-empty, only
	// checkpoints for that specific run are considered.
	Load(graphName, runID string) (*Checkpoint, error)
}

// CheckpointManager extends CheckpointStore with lifecycle operations.
// FileCheckpointStore implements this interface.
type CheckpointManager interface {
	CheckpointStore
	List() ([]string, error)
	Delete(graphName string) error
}

// CheckpointCleaner supports periodic cleanup of old checkpoints.
type CheckpointCleaner interface {
	Cleanup(opts CleanupOptions) (deleted int, err error)
}

// CleanupOptions configures checkpoint cleanup behavior.
type CleanupOptions struct {
	MaxAge   time.Duration
	MaxCount int
}

// ---------- FileCheckpointStore ----------

// FileCheckpointConfig configures the file-based checkpoint store.
type FileCheckpointConfig struct {
	Dir            string
	MaxCheckpoints int
}

// FileCheckpointStore persists checkpoints as JSON files.
type FileCheckpointStore struct {
	dir            string
	maxCheckpoints int
}

// NewFileCheckpointStore creates a file-based checkpoint store.
func NewFileCheckpointStore(cfg FileCheckpointConfig) (*FileCheckpointStore, error) {
	if cfg.MaxCheckpoints <= 0 {
		cfg.MaxCheckpoints = 3
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("checkpoint: create dir: %w", err)
	}
	return &FileCheckpointStore{dir: cfg.Dir, maxCheckpoints: cfg.MaxCheckpoints}, nil
}

func (s *FileCheckpointStore) filename(graphName, runID string) string {
	if runID != "" {
		return filepath.Join(s.dir, graphName+"."+runID+".checkpoint.json")
	}
	return filepath.Join(s.dir, graphName+".checkpoint.json")
}

// Save writes a checkpoint to disk with multi-version retention.
func (s *FileCheckpointStore) Save(cp Checkpoint) error {
	primary := s.filename(cp.GraphName, cp.RunID)

	if _, err := os.Stat(primary); err == nil {
		ts := time.Now().Format("20060102T150405")
		backup := filepath.Join(s.dir,
			fmt.Sprintf("%s.checkpoint.%s.json", cp.GraphName, ts))
		_ = os.Rename(primary, backup)
	}

	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("checkpoint: marshal: %w", err)
	}
	if err := os.WriteFile(primary, data, 0o644); err != nil {
		return fmt.Errorf("checkpoint: write: %w", err)
	}

	s.cleanOldVersions(cp)
	return nil
}

// Load reads the latest checkpoint from disk.
func (s *FileCheckpointStore) Load(graphName, runID string) (*Checkpoint, error) {
	data, err := os.ReadFile(s.filename(graphName, runID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("checkpoint: read: %w", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("checkpoint: unmarshal: %w", err)
	}
	return &cp, nil
}

// List returns graph names that have a primary checkpoint file.
// Primary files match "{name}.checkpoint.json"; backup files have an
// additional timestamp segment ("{name}.checkpoint.{ts}.json") and are skipped.
func (s *FileCheckpointStore) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("checkpoint: list dir: %w", err)
	}
	const suffix = ".checkpoint.json"
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, suffix) {
			continue
		}
		base := strings.TrimSuffix(name, suffix)
		// Backup files contain an extra ".checkpoint.{ts}" segment, so
		// their base would itself contain ".checkpoint." — skip those.
		if strings.Contains(base, ".checkpoint.") {
			continue
		}
		names = append(names, base)
	}
	return names, nil
}

// Delete removes the primary checkpoint and all backups for the given graph.
func (s *FileCheckpointStore) Delete(graphName string) error {
	primary := s.filename(graphName, "")
	_ = os.Remove(primary)
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	prefix := graphName + ".checkpoint."
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
	return nil
}

// Cleanup removes stale checkpoint files.
func (s *FileCheckpointStore) Cleanup(opts CleanupOptions) (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("checkpoint: read dir: %w", err)
	}

	type cpEntry struct {
		path    string
		modTime time.Time
	}
	var cps []cpEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cps = append(cps, cpEntry{path: filepath.Join(s.dir, e.Name()), modTime: info.ModTime()})
	}

	deleted := 0
	if opts.MaxAge > 0 {
		cutoff := time.Now().Add(-opts.MaxAge)
		remaining := cps[:0]
		for _, cp := range cps {
			if cp.modTime.Before(cutoff) {
				if err := os.Remove(cp.path); err == nil {
					deleted++
				}
			} else {
				remaining = append(remaining, cp)
			}
		}
		cps = remaining
	}

	if opts.MaxCount > 0 && len(cps) > opts.MaxCount {
		sort.Slice(cps, func(i, j int) bool {
			return cps[i].modTime.After(cps[j].modTime)
		})
		for _, cp := range cps[opts.MaxCount:] {
			if err := os.Remove(cp.path); err == nil {
				deleted++
			}
		}
	}

	return deleted, nil
}

func (s *FileCheckpointStore) cleanOldVersions(cp Checkpoint) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}

	var prefix string
	if cp.RunID != "" {
		prefix = cp.GraphName + "." + cp.RunID + ".checkpoint."
	} else {
		prefix = cp.GraphName + ".checkpoint."
	}
	primaryName := filepath.Base(s.filename(cp.GraphName, cp.RunID))
	type versioned struct {
		path    string
		modTime time.Time
	}
	var versions []versioned
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) && name != primaryName {
			info, err := e.Info()
			if err != nil {
				continue
			}
			versions = append(versions, versioned{
				path:    filepath.Join(s.dir, name),
				modTime: info.ModTime(),
			})
		}
	}

	if len(versions) <= s.maxCheckpoints {
		return
	}
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].modTime.After(versions[j].modTime)
	})
	for _, v := range versions[s.maxCheckpoints:] {
		_ = os.Remove(v.path)
	}
}

// storeOnlyHost adapts a deprecated executor.CheckpointStore into a
// full engine.Host so the executor's main path can rely on a single
// Checkpoint sink (host.Checkpoint) without branching on store vs host.
//
// It mirrors busOnlyHost (the runner.WithEventBus shim): every Host
// method other than Checkpoint is inherited from the embedded base
// host (typically engine.NoopHost{}). Checkpoint converts the engine
// canonical form back to the legacy struct and forwards to the store.
//
// Deprecated: scheduled for removal in v0.3.0 together with
// executor.WithCheckpointStore. New code should pass a real
// engine.Host whose Checkpointer talks to engine.CheckpointStore
// directly.
type storeOnlyHost struct {
	engine.Host
	store CheckpointStore
}

// Checkpoint forwards to the wrapped legacy store. Errors are
// swallowed at the call site (executor) by convention so a failing
// observer never aborts a run; the host contract here just propagates
// whatever the store reported so callers that wrap us can still log.
func (h storeOnlyHost) Checkpoint(_ context.Context, cp engine.Checkpoint) error {
	if h.store == nil {
		return nil
	}
	return h.store.Save(checkpointFromEngine(cp))
}

// resolveCheckpointHost folds a legacy CheckpointStore into the
// modern host so the executor only has one Checkpointer to call.
//
// Resolution rules (mirroring resolvePublisher):
//   - host alone: use it as-is.
//   - store alone: wrap in storeOnlyHost{NoopHost, store}.
//   - both: host wins; the legacy store is ignored. Checkpointing is
//     state, not observability — writing to two backends invites
//     conflicting reads, so we deliberately do NOT fan out the way
//     resolvePublisher does for envelopes. The deprecation comment on
//     WithCheckpointStore tells callers to drop it once they wire a
//     real host.
//   - neither: host stays NoopHost{}, Checkpoint becomes a no-op.
//
// Returned host is always non-nil so the executor can call
// host.Checkpoint unconditionally.
func resolveCheckpointHost(host engine.Host, store CheckpointStore) engine.Host {
	switch {
	case host == nil && store == nil:
		return engine.NoopHost{}
	case store == nil:
		return host
	case host == nil:
		return storeOnlyHost{Host: engine.NoopHost{}, store: store}
	default:
		return host
	}
}
