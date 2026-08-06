// Package sources contains shared durable source-discovery contracts.
package sources

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const scopeCatalogSchemaVersion = 1

// ScopeCatalog is the runtime authority for source partitions discoverable by
// workers. Register is idempotent.
type ScopeCatalog interface {
	Register(context.Context, sdkmemory.Scope) error
	List(context.Context) ([]sdkmemory.Scope, error)
}

// WorkspaceScopeCatalog stores one immutable, schema-versioned entry per
// scope. Independent entries avoid a shared manifest read-modify-write race.
type WorkspaceScopeCatalog struct {
	ws workspace.Workspace
	mu sync.RWMutex
}

type persistedScope struct {
	SchemaVersion int             `json:"schema_version"`
	Scope         sdkmemory.Scope `json:"scope"`
}

var _ ScopeCatalog = (*WorkspaceScopeCatalog)(nil)

func NewWorkspaceScopeCatalog(ws workspace.Workspace) (*WorkspaceScopeCatalog, error) {
	if nilWorkspace(ws) {
		return nil, errors.New("memory source catalog: workspace is required")
	}
	return &WorkspaceScopeCatalog{ws: ws}, nil
}

func (catalog *WorkspaceScopeCatalog) Register(ctx context.Context, scope sdkmemory.Scope) error {
	if catalog == nil || nilWorkspace(catalog.ws) {
		return errors.New("memory source catalog: catalog is required")
	}
	if ctx == nil {
		return errors.New("memory source catalog: context is required")
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	value := persistedScope{SchemaVersion: scopeCatalogSchemaVersion, Scope: scope}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("memory source catalog: encode scope: %w", err)
	}

	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	target := catalog.entryPath(scope)
	existing, err := catalog.ws.Read(ctx, target)
	if err == nil {
		var prior persistedScope
		if err := decodeStrict(existing, &prior); err != nil {
			return fmt.Errorf("memory source catalog: decode existing scope: %w", err)
		}
		if err := validatePersistedScope(prior, scope.HardPartitionKey()); err != nil {
			return fmt.Errorf("memory source catalog: corrupt existing scope: %w", err)
		}
		if !reflect.DeepEqual(prior, value) {
			return errors.New("memory source catalog: immutable scope entry conflicts")
		}
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("memory source catalog: inspect scope: %w", err)
	}
	if err := workspace.AtomicWrite(ctx, catalog.ws, target, data); err != nil {
		existing, readErr := catalog.ws.Read(ctx, target)
		if readErr == nil {
			var prior persistedScope
			if decodeErr := decodeStrict(existing, &prior); decodeErr == nil &&
				validatePersistedScope(prior, scope.HardPartitionKey()) == nil &&
				reflect.DeepEqual(prior, value) {
				return nil
			}
		}
		return fmt.Errorf("memory source catalog: publish scope: %w", err)
	}
	return nil
}

func (catalog *WorkspaceScopeCatalog) List(ctx context.Context) ([]sdkmemory.Scope, error) {
	if catalog == nil || nilWorkspace(catalog.ws) {
		return nil, errors.New("memory source catalog: catalog is required")
	}
	if ctx == nil {
		return nil, errors.New("memory source catalog: context is required")
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	entries, err := catalog.ws.List(ctx, catalog.entriesDir())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []sdkmemory.Scope{}, nil
		}
		return nil, fmt.Errorf("memory source catalog: list scopes: %w", err)
	}
	scopes := make([]sdkmemory.Scope, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("memory source catalog: unexpected directory %q", entry.Name())
		}
		key, dataName, err := decodeFilename(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("memory source catalog: decode entry name %q: %w", entry.Name(), err)
		}
		if !dataName {
			continue
		}
		data, err := catalog.ws.Read(ctx, path.Join(catalog.entriesDir(), entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("memory source catalog: read scope %q: %w", key, err)
		}
		var value persistedScope
		if err := decodeStrict(data, &value); err != nil {
			return nil, fmt.Errorf("memory source catalog: decode scope %q: %w", key, err)
		}
		if err := validatePersistedScope(value, key); err != nil {
			return nil, fmt.Errorf("memory source catalog: corrupt scope %q: %w", key, err)
		}
		scopes = append(scopes, value.Scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		return scopes[i].HardPartitionKey() < scopes[j].HardPartitionKey()
	})
	return scopes, nil
}

func validatePersistedScope(value persistedScope, key string) error {
	if value.SchemaVersion != scopeCatalogSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", value.SchemaVersion)
	}
	if err := value.Scope.Validate(); err != nil {
		return err
	}
	if value.Scope.HardPartitionKey() != key {
		return errors.New("persisted scope does not match workspace path")
	}
	return nil
}

func (catalog *WorkspaceScopeCatalog) entriesDir() string {
	return path.Join("sources", "catalog", "v1", "scopes")
}

func (catalog *WorkspaceScopeCatalog) entryPath(scope sdkmemory.Scope) string {
	return path.Join(catalog.entriesDir(), encode(scope.HardPartitionKey())+".json")
}

func encode(value string) string {
	return "k_" + base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeFilename(name string) (string, bool, error) {
	if !strings.HasSuffix(name, ".json") {
		return "", false, nil
	}
	segment := strings.TrimSuffix(name, ".json")
	if !strings.HasPrefix(segment, "k_") {
		return "", false, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, "k_"))
	if err != nil {
		return "", true, err
	}
	if encode(string(data)) != segment {
		return "", true, errors.New("non-canonical path segment")
	}
	return string(data), true, nil
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
