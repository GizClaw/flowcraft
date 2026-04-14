package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/graph"
)

// Checkpoint represents a snapshot of graph execution state.
type Checkpoint struct {
	GraphName string               `json:"graph_name"`
	RunID     string               `json:"run_id,omitempty"`
	NodeID    string               `json:"node_id"`
	Iteration int                  `json:"iteration"`
	Board     *graph.BoardSnapshot `json:"board"`
	Timestamp time.Time            `json:"timestamp"`
}

// CheckpointStore is the interface for persisting and loading checkpoints.
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
