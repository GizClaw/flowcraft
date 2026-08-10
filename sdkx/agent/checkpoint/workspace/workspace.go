// Package workspace provides an [agent.CheckpointStore] backed by a
// [workspace.Workspace]. Checkpoints are stored as JSON files under a
// configurable prefix, one file per exec id.
//
// Save publishes through a temporary file + Rename so readers never
// observe a half-written checkpoint, even on backends whose Write is
// not atomic.
package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const defaultPrefix = "agent/checkpoints"

// Store is a workspace-backed [agent.CheckpointStore]. It also
// implements [agent.CheckpointLister] and [agent.CheckpointDeleter].
// All methods are safe for concurrent use when the underlying
// workspace implementation is.
type Store struct {
	ws     workspace.Workspace
	prefix string
}

type options struct {
	prefix string
}

// Option configures a Store.
type Option func(*options)

// WithPrefix sets the workspace directory holding checkpoints.
// Leading and trailing slashes are normalized away. The default is
// "agent/checkpoints".
func WithPrefix(prefix string) Option {
	return func(o *options) {
		o.prefix = strings.Trim(prefix, "/")
	}
}

// New constructs a checkpoint store over ws. The workspace is
// borrowed: the caller keeps its lifecycle.
func New(ws workspace.Workspace, opts ...Option) (*Store, error) {
	if nilWorkspace(ws) {
		return nil, errors.New("workspace checkpoint: workspace is required")
	}
	o := options{prefix: defaultPrefix}
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	return &Store{ws: ws, prefix: strings.Trim(o.prefix, "/")}, nil
}

// Save implements [agent.CheckpointStore].
func (s *Store) Save(ctx context.Context, cp agent.Checkpoint) error {
	if s == nil || nilWorkspace(s.ws) {
		return errors.New("workspace checkpoint: store is not initialized")
	}
	if err := cp.Validate(); err != nil {
		return err
	}
	if err := validateExecID(cp.ExecID); err != nil {
		return err
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("workspace checkpoint: encode: %w", err)
	}
	tmp := s.tmpPath(cp.ExecID)
	if err := s.ws.Write(ctx, tmp, data); err != nil {
		return fmt.Errorf("workspace checkpoint: write temp: %w", err)
	}
	if err := s.ws.Rename(ctx, tmp, s.livePath(cp.ExecID)); err != nil {
		_ = s.ws.Delete(ctx, tmp)
		return fmt.Errorf("workspace checkpoint: publish %s: %w", cp.ExecID, err)
	}
	return nil
}

// Load implements [agent.CheckpointStore].
func (s *Store) Load(ctx context.Context, execID string) (*agent.Checkpoint, error) {
	if s == nil || nilWorkspace(s.ws) {
		return nil, errors.New("workspace checkpoint: store is not initialized")
	}
	if err := validateExecID(execID); err != nil {
		return nil, err
	}
	data, err := s.ws.Read(ctx, s.livePath(execID))
	if err != nil {
		if errors.Is(err, workspace.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("workspace checkpoint: read %s: %w", execID, err)
	}
	var cp agent.Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("workspace checkpoint: decode %s: %w", execID, err)
	}
	if err := cp.Validate(); err != nil {
		return nil, fmt.Errorf("workspace checkpoint: corrupt %s: %w", execID, err)
	}
	return &cp, nil
}

// List implements [agent.CheckpointLister].
func (s *Store) List(ctx context.Context) ([]string, error) {
	if s == nil || nilWorkspace(s.ws) {
		return nil, errors.New("workspace checkpoint: store is not initialized")
	}
	entries, err := s.ws.List(ctx, s.dir())
	if err != nil {
		return nil, fmt.Errorf("workspace checkpoint: list: %w", err)
	}
	ids := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(entry.Name(), ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

// Delete implements [agent.CheckpointDeleter].
func (s *Store) Delete(ctx context.Context, execID string) error {
	if s == nil || nilWorkspace(s.ws) {
		return errors.New("workspace checkpoint: store is not initialized")
	}
	if err := validateExecID(execID); err != nil {
		return err
	}
	if err := s.ws.Delete(ctx, s.livePath(execID)); err != nil {
		return fmt.Errorf("workspace checkpoint: delete %s: %w", execID, err)
	}
	return nil
}

func (s *Store) livePath(execID string) string {
	return path.Join(s.prefix, execID+".json")
}

func (s *Store) tmpPath(execID string) string {
	return path.Join(s.prefix, ".tmp", execID+"."+randomSuffix()+".json.tmp")
}

func (s *Store) dir() string {
	if s.prefix == "" {
		return "."
	}
	return s.prefix
}

func validateExecID(execID string) error {
	if strings.TrimSpace(execID) == "" ||
		strings.ContainsAny(execID, "/\\") ||
		execID == "." || execID == ".." {
		return errdefs.Validationf(
			"workspace checkpoint: exec_id %q is not a valid file segment", execID)
	}
	return nil
}

func randomSuffix() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes[:])
}

func nilWorkspace(ws workspace.Workspace) bool {
	if ws == nil {
		return true
	}
	value := reflect.ValueOf(ws)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Compile-time assertions that Store satisfies the checkpoint store
// contract plus the optional extensions.
var (
	_ agent.CheckpointStore   = (*Store)(nil)
	_ agent.CheckpointLister  = (*Store)(nil)
	_ agent.CheckpointDeleter = (*Store)(nil)
)
