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

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const (
	checkpointSchemaVersion = 1
	watermarkRoot           = "worker/v1/watermarks"
)

// SourceWatermark is the durable, policy-scoped cursor for one canonical
// source stream. The cursor advances only after every derived write for the
// source item completed.
type SourceWatermark struct {
	Scope        corememory.Scope `json:"scope"`
	StreamKind   string           `json:"stream_kind"`
	StreamID     string           `json:"stream_id"`
	PolicyDigest string           `json:"policy_digest"`
	Cursor       uint64           `json:"cursor"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

// CheckpointStore persists derivation progress for one policy.
type CheckpointStore interface {
	LoadWatermark(context.Context, corememory.Scope, string, string, string) (SourceWatermark, bool, error)
	SaveWatermark(context.Context, SourceWatermark) error
}

// KVCheckpoints persists watermarks on a storage.Store.
type KVCheckpoints struct {
	kv    storage.Store
	clock func() time.Time
}

// Option configures the checkpoint store.
type Option func(*KVCheckpoints)

// WithClock replaces the clock used for watermark timestamps.
func WithClock(clock func() time.Time) Option {
	return func(store *KVCheckpoints) {
		if clock != nil {
			store.clock = clock
		}
	}
}

// NewKVCheckpoints constructs a KV-backed checkpoint store.
func NewKVCheckpoints(kv storage.Store, options ...Option) (*KVCheckpoints, error) {
	if nilInterface(kv) {
		return nil, errors.New("memory worker: kv store is required")
	}
	store := &KVCheckpoints{kv: kv, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

// LoadWatermark returns the stored cursor for one stream inside one policy.
func (store *KVCheckpoints) LoadWatermark(
	ctx context.Context,
	scope corememory.Scope,
	streamKind, streamID, policyDigest string,
) (SourceWatermark, bool, error) {
	if store == nil || nilInterface(store.kv) {
		return SourceWatermark{}, false, errors.New("memory worker: checkpoint store is incomplete")
	}
	if ctx == nil {
		return SourceWatermark{}, false, errors.New("memory worker: context is required")
	}
	key, err := watermarkKey(scope, streamKind, streamID, policyDigest)
	if err != nil {
		return SourceWatermark{}, false, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return SourceWatermark{}, false, nil
		}
		return SourceWatermark{}, false, err
	}
	var watermark SourceWatermark
	if err := decodeStrict(data, &watermark); err != nil {
		return SourceWatermark{}, false, fmt.Errorf("memory worker: decode watermark: %w", err)
	}
	if err := validateWatermark(watermark, key); err != nil {
		return SourceWatermark{}, false, err
	}
	return watermark, true, nil
}

// SaveWatermark publishes one cursor, overwriting the previous value.
func (store *KVCheckpoints) SaveWatermark(ctx context.Context, watermark SourceWatermark) error {
	if store == nil || nilInterface(store.kv) {
		return errors.New("memory worker: checkpoint store is incomplete")
	}
	if ctx == nil {
		return errors.New("memory worker: context is required")
	}
	if watermark.UpdatedAt.IsZero() {
		watermark.UpdatedAt = store.clock().UTC()
	}
	key, err := watermarkKey(watermark.Scope, watermark.StreamKind, watermark.StreamID, watermark.PolicyDigest)
	if err != nil {
		return err
	}
	if err := validateWatermark(watermark, key); err != nil {
		return err
	}
	data, err := json.Marshal(watermark)
	if err != nil {
		return fmt.Errorf("memory worker: encode watermark: %w", err)
	}
	if err := store.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("memory worker: save watermark: %w", err)
	}
	return nil
}

func watermarkKey(scope corememory.Scope, streamKind, streamID, policyDigest string) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if strings.TrimSpace(streamKind) == "" || strings.TrimSpace(streamID) == "" {
		return "", errors.New("memory worker: stream kind and id are required")
	}
	if strings.TrimSpace(policyDigest) == "" {
		return "", errors.New("memory worker: policy digest is required")
	}
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return watermarkRoot + "/" + partition +
		"/" + storage.EncodeSegment(streamKind) +
		"/" + storage.EncodeSegment(streamID) +
		"/" + storage.EncodeSegment(policyDigest), nil
}

func validateWatermark(watermark SourceWatermark, key string) error {
	if watermark.Cursor == 0 {
		return errors.New("memory worker: watermark cursor must be positive")
	}
	if err := watermark.Scope.Validate(); err != nil {
		return err
	}
	expected, err := watermarkKey(watermark.Scope, watermark.StreamKind, watermark.StreamID, watermark.PolicyDigest)
	if err != nil {
		return err
	}
	if expected != key {
		return errors.New("memory worker: persisted watermark does not match its key")
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
