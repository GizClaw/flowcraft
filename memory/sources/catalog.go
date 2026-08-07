// Package sources contains shared durable source-discovery contracts.
package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/memory/storage"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

const (
	scopeCatalogSchemaVersion = 1
	catalogRoot               = "sources/v1/catalog"
)

// ScopeCatalog is the runtime authority for source partitions discoverable by
// workers. Register is idempotent.
type ScopeCatalog struct {
	kv storage.Store
	mu sync.Mutex
}

type persistedScope struct {
	SchemaVersion int             `json:"schema_version"`
	Scope         sdkmemory.Scope `json:"scope"`
}

// NewScopeCatalog constructs a KV-backed scope catalog.
func NewScopeCatalog(kv storage.Store) (*ScopeCatalog, error) {
	if nilInterface(kv) {
		return nil, errors.New("memory source catalog: store is required")
	}
	if _, ok := kv.(storage.PutIfAbsentStore); !ok {
		return nil, errors.New("memory source catalog: store must support immutable writes")
	}
	return &ScopeCatalog{kv: kv}, nil
}

// Register records one immutable, schema-versioned entry per scope.
func (catalog *ScopeCatalog) Register(ctx context.Context, scope sdkmemory.Scope) error {
	if catalog == nil || nilInterface(catalog.kv) {
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
	key, err := catalogKey(scope)
	if err != nil {
		return err
	}
	put, ok := catalog.kv.(storage.PutIfAbsentStore)
	if !ok {
		return errors.New("memory source catalog: store must support immutable writes")
	}
	written, err := put.PutIfAbsent(ctx, key, data)
	if err != nil {
		return err
	}
	if written {
		return nil
	}
	existing, err := catalog.kv.Get(ctx, key)
	if err != nil {
		return err
	}
	var prior persistedScope
	if err := decodeStrict(existing, &prior); err != nil {
		return fmt.Errorf("memory source catalog: decode existing scope: %w", err)
	}
	if err := validatePersistedScope(prior, key); err != nil {
		return fmt.Errorf("memory source catalog: corrupt existing scope: %w", err)
	}
	if !reflect.DeepEqual(prior, value) {
		return errors.New("memory source catalog: immutable scope entry conflicts")
	}
	return nil
}

// List returns every registered scope in hard-partition order.
func (catalog *ScopeCatalog) List(ctx context.Context) ([]sdkmemory.Scope, error) {
	if catalog == nil || nilInterface(catalog.kv) {
		return nil, errors.New("memory source catalog: catalog is required")
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	entries, err := catalog.kv.List(ctx, catalogRoot)
	if err != nil {
		return nil, err
	}
	scopes := make([]sdkmemory.Scope, 0, len(entries))
	for _, entry := range entries {
		var value persistedScope
		if err := decodeStrict(entry.Value, &value); err != nil {
			return nil, fmt.Errorf("memory source catalog: decode scope %q: %w", entry.Key, err)
		}
		if err := validatePersistedScope(value, entry.Key); err != nil {
			return nil, fmt.Errorf("memory source catalog: corrupt scope %q: %w", entry.Key, err)
		}
		scopes = append(scopes, value.Scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		return scopes[i].HardPartitionKey() < scopes[j].HardPartitionKey()
	})
	return scopes, nil
}

func catalogKey(scope sdkmemory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return catalogRoot + "/" + partition, nil
}

func validatePersistedScope(value persistedScope, key string) error {
	if value.SchemaVersion != scopeCatalogSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", value.SchemaVersion)
	}
	if err := value.Scope.Validate(); err != nil {
		return err
	}
	expected, err := catalogKey(value.Scope)
	if err != nil {
		return err
	}
	if expected != key {
		return errors.New("persisted scope does not match catalog key")
	}
	return nil
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
