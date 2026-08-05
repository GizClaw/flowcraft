// Package projection provides the shared immutable-build publication protocol.
package projection

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const (
	SchemaVersion           = 2
	StorageAlgorithmVersion = "workspace-lsm-v1"
	DefaultMaxSegments      = 32
	DefaultMaxDeltaBytes    = 4 << 20
)

type Thresholds struct {
	MaxSegments   int   `json:"max_segments"`
	MaxDeltaBytes int64 `json:"max_delta_bytes"`
}

func DefaultThresholds() Thresholds {
	return Thresholds{MaxSegments: DefaultMaxSegments, MaxDeltaBytes: DefaultMaxDeltaBytes}
}

func (thresholds Thresholds) normalized() (Thresholds, error) {
	if thresholds.MaxSegments == 0 {
		thresholds.MaxSegments = DefaultMaxSegments
	}
	if thresholds.MaxDeltaBytes == 0 {
		thresholds.MaxDeltaBytes = DefaultMaxDeltaBytes
	}
	if thresholds.MaxSegments < 1 || thresholds.MaxDeltaBytes < 1 {
		return Thresholds{}, errors.New("projection: compaction thresholds must be positive")
	}
	return thresholds, nil
}

type BaseRef struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
	Bytes  int64  `json:"bytes"`
}

type SegmentRef struct {
	ID               string `json:"id"`
	Digest           string `json:"digest"`
	PreviousIdentity string `json:"previous_identity"`
	SourceRevision   string `json:"source_revision,omitempty"`
	DeltaDigest      string `json:"delta_digest"`
	SourceInput      string `json:"source_input"`
	Bytes            int64  `json:"bytes"`
}

type ReplayRef struct {
	SourceRevision string `json:"source_revision,omitempty"`
	DeltaDigest    string `json:"delta_digest"`
	SourceInput    string `json:"source_input"`
}

// Manifest is the sole active pointer. It is bounded by compaction thresholds,
// never by the number of indexed entries.
type Manifest struct {
	SchemaVersion    int             `json:"schema_version"`
	StorageVersion   string          `json:"storage_version"`
	Projection       string          `json:"projection"`
	Scope            sdkmemory.Scope `json:"scope"`
	Base             *BaseRef        `json:"base,omitempty"`
	Segments         []SegmentRef    `json:"segments"`
	BaseSourceDigest string          `json:"base_source_digest"`
	SourceDigest     string          `json:"source_digest"`
	BuildDigest      string          `json:"build_digest"`
	BaseLastApplied  *ReplayRef      `json:"base_last_applied,omitempty"`
	LastApplied      *ReplayRef      `json:"last_applied,omitempty"`
	Identity         string          `json:"identity"`
}

type DigestEvidence struct {
	StoredSourceDigest   string
	ComputedSourceDigest string
	StoredBuildDigest    string
	ComputedBuildDigest  string
}

type segment[D any] struct {
	SchemaVersion    int             `json:"schema_version"`
	StorageVersion   string          `json:"storage_version"`
	Projection       string          `json:"projection"`
	Scope            sdkmemory.Scope `json:"scope"`
	ID               string          `json:"id"`
	PreviousIdentity string          `json:"previous_identity"`
	SourceRevision   string          `json:"source_revision,omitempty"`
	DeltaDigest      string          `json:"delta_digest"`
	SourceInput      string          `json:"source_input"`
	SourceDigest     string          `json:"source_digest"`
	BuildDigest      string          `json:"build_digest"`
	Delta            D               `json:"delta"`
}

type TypedOptions[B, D any] struct {
	Thresholds    Thresholds
	Canonicalize  func(D) D
	ValidateBase  func(B) error
	ValidateDelta func(D) error
	Apply         func(*B, D) error
}

type materializedCache[B any] struct {
	identity string
	value    B
}

// Store owns the common typed immutable-base and ordered-segment protocol.
type Store[B, D any] struct {
	Workspace workspace.Workspace
	Family    string
	options   TypedOptions[B, D]
	mu        sync.Mutex
	cache     *materializedCache[B]
}

type ApplyResult struct {
	Manifest  Manifest
	Replayed  bool
	Compacted bool
}

func NewTypedStore[B, D any](ws workspace.Workspace, family string, options TypedOptions[B, D]) (*Store[B, D], error) {
	if nilInterface(ws) {
		return nil, errors.New("projection: workspace is required")
	}
	if strings.TrimSpace(family) == "" {
		return nil, errors.New("projection: family is required")
	}
	thresholds, err := options.Thresholds.normalized()
	if err != nil {
		return nil, err
	}
	if options.Apply == nil {
		return nil, errors.New("projection: typed delta apply function is required")
	}
	options.Thresholds = thresholds
	return &Store[B, D]{Workspace: ws, Family: family, options: options}, nil
}

func (store *Store[B, D]) FullRebuild(ctx context.Context, scope sdkmemory.Scope, name string, base B, sourceDigest string) error {
	if err := validateAddress(scope, name); err != nil {
		return err
	}
	if store.options.ValidateBase != nil {
		if err := store.options.ValidateBase(base); err != nil {
			return fmt.Errorf("projection: validate base: %w", err)
		}
	}
	payload, err := json.Marshal(base)
	if err != nil {
		return fmt.Errorf("projection: encode base: %w", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.publishBaseLocked(ctx, scope, name, payload, sourceDigest, nil)
}

func (store *Store[B, D]) ApplyDelta(
	ctx context.Context,
	scope sdkmemory.Scope,
	name string,
	delta D,
	sourceRevision string,
	sourceInput string,
) (ApplyResult, error) {
	if err := validateAddress(scope, name); err != nil {
		return ApplyResult{}, err
	}
	if store.options.Canonicalize != nil {
		delta = store.options.Canonicalize(delta)
	}
	if store.options.ValidateDelta != nil {
		if err := store.options.ValidateDelta(delta); err != nil {
			return ApplyResult{}, fmt.Errorf("projection: validate delta: %w", err)
		}
	}
	deltaPayload, err := json.Marshal(delta)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("projection: encode delta: %w", err)
	}
	deltaDigest := digestBytes(deltaPayload)
	if sourceInput == "" {
		sourceInput = deltaDigest
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	active, found, err := store.loadManifestLocked(ctx, scope, name)
	if err != nil {
		return ApplyResult{}, err
	}
	if !found {
		active = newManifest(scope, name)
	}
	if active.LastApplied != nil {
		last := active.LastApplied
		if last.SourceRevision == sourceRevision && last.DeltaDigest == deltaDigest && last.SourceInput == sourceInput {
			return ApplyResult{Manifest: active, Replayed: true}, nil
		}
	}
	nextSource := chainDigest("source", active.SourceDigest, sourceInput)
	segmentID := chainDigest("segment", active.Identity, sourceRevision, deltaDigest, sourceInput)
	nextBuild := chainDigest("build", active.BuildDigest, segmentID)
	value := segment[D]{
		SchemaVersion: SchemaVersion, StorageVersion: StorageAlgorithmVersion,
		Projection: name, Scope: scope, ID: segmentID, PreviousIdentity: active.Identity,
		SourceRevision: sourceRevision, DeltaDigest: deltaDigest, SourceInput: sourceInput,
		SourceDigest: nextSource, BuildDigest: nextBuild, Delta: delta,
	}
	segmentPayload, err := json.Marshal(value)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("projection: encode segment: %w", err)
	}
	totalBytes := int64(len(segmentPayload))
	for _, ref := range active.Segments {
		totalBytes += ref.Bytes
	}
	if len(active.Segments)+1 > store.options.Thresholds.MaxSegments ||
		totalBytes > store.options.Thresholds.MaxDeltaBytes {
		base, err := store.materializeLocked(ctx, scope, name, active)
		if err != nil {
			return ApplyResult{}, err
		}
		if err := store.options.Apply(&base, delta); err != nil {
			return ApplyResult{}, fmt.Errorf("projection: compact delta: %w", err)
		}
		if store.options.ValidateBase != nil {
			if err := store.options.ValidateBase(base); err != nil {
				return ApplyResult{}, fmt.Errorf("projection: validate compacted base: %w", err)
			}
		}
		payload, err := json.Marshal(base)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("projection: encode compacted base: %w", err)
		}
		replay := &ReplayRef{SourceRevision: sourceRevision, DeltaDigest: deltaDigest, SourceInput: sourceInput}
		if err := store.publishBaseLocked(ctx, scope, name, payload, nextSource, replay); err != nil {
			return ApplyResult{}, err
		}
		compacted, _, err := store.loadManifestLocked(ctx, scope, name)
		return ApplyResult{Manifest: compacted, Compacted: true}, err
	}
	if err := store.writeImmutable(ctx, store.segmentPath(scope, name, segmentID), segmentPayload); err != nil {
		return ApplyResult{}, fmt.Errorf("projection: write immutable segment: %w", err)
	}
	active.Segments = append(active.Segments, SegmentRef{
		ID: segmentID, Digest: digestBytes(segmentPayload), PreviousIdentity: active.Identity,
		SourceRevision: sourceRevision, DeltaDigest: deltaDigest, SourceInput: sourceInput,
		Bytes: int64(len(segmentPayload)),
	})
	active.SourceDigest = nextSource
	active.BuildDigest = nextBuild
	active.LastApplied = &ReplayRef{
		SourceRevision: sourceRevision, DeltaDigest: deltaDigest, SourceInput: sourceInput,
	}
	active.Identity = manifestIdentity(active)
	if err := store.publishManifest(ctx, scope, name, active); err != nil {
		return ApplyResult{}, err
	}
	store.cache = nil
	return ApplyResult{Manifest: active}, nil
}

func (store *Store[B, D]) Materialize(ctx context.Context, scope sdkmemory.Scope, name string) (B, Manifest, error) {
	var zero B
	if err := validateAddress(scope, name); err != nil {
		return zero, Manifest{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	active, found, err := store.loadManifestLocked(ctx, scope, name)
	if err != nil {
		return zero, Manifest{}, err
	}
	if !found {
		return zero, Manifest{}, fmt.Errorf("projection: read active manifest: %w", workspace.ErrNotFound)
	}
	base, err := store.materializeLocked(ctx, scope, name, active)
	return base, active, err
}

func (store *Store[B, D]) Audit(ctx context.Context, scope sdkmemory.Scope, name string) (Manifest, bool, error) {
	if err := validateAddress(scope, name); err != nil {
		return Manifest{}, false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	active, found, err := store.loadManifestLocked(ctx, scope, name)
	if err != nil || !found {
		return Manifest{}, found, err
	}
	_, err = store.materializeUncachedLocked(ctx, scope, name, active)
	return active, true, err
}

func (store *Store[B, D]) AuditDigestEvidence(
	ctx context.Context,
	scope sdkmemory.Scope,
	name string,
) (DigestEvidence, bool, error) {
	if err := validateAddress(scope, name); err != nil {
		return DigestEvidence{}, false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	active, found, err := store.loadManifestForAuditLocked(ctx, scope, name)
	if err != nil || !found {
		return DigestEvidence{}, found, err
	}
	_, sourceDigest, buildDigest, identity, err := store.materializeAuditLocked(ctx, scope, name, active)
	if err != nil {
		return DigestEvidence{}, true, err
	}
	if sourceDigest == active.SourceDigest && buildDigest == active.BuildDigest && identity != active.Identity {
		return DigestEvidence{}, true, errors.New("projection: active manifest identity mismatch")
	}
	return DigestEvidence{
		StoredSourceDigest: active.SourceDigest, ComputedSourceDigest: sourceDigest,
		StoredBuildDigest: active.BuildDigest, ComputedBuildDigest: buildDigest,
	}, true, nil
}

func (store *Store[B, D]) materializeLocked(ctx context.Context, scope sdkmemory.Scope, name string, active Manifest) (B, error) {
	if store.cache != nil && store.cache.identity == active.Identity {
		return cloneTyped(store.cache.value)
	}
	value, err := store.materializeUncachedLocked(ctx, scope, name, active)
	if err != nil {
		var zero B
		return zero, err
	}
	cached, err := cloneTyped(value)
	if err != nil {
		var zero B
		return zero, err
	}
	store.cache = &materializedCache[B]{identity: active.Identity, value: cached}
	return cloneTyped(value)
}

func (store *Store[B, D]) materializeUncachedLocked(ctx context.Context, scope sdkmemory.Scope, name string, active Manifest) (B, error) {
	value, sourceDigest, buildDigest, identity, err := store.materializeAuditLocked(ctx, scope, name, active)
	if err != nil {
		return value, err
	}
	if sourceDigest != active.SourceDigest || buildDigest != active.BuildDigest || identity != active.Identity {
		return value, errors.New("projection: active manifest cumulative digest or identity mismatch")
	}
	return value, nil
}

func (store *Store[B, D]) materializeAuditLocked(
	ctx context.Context,
	scope sdkmemory.Scope,
	name string,
	active Manifest,
) (B, string, string, string, error) {
	var value B
	buildDigest := ""
	if active.Base != nil {
		payload, err := store.Workspace.Read(ctx, store.buildPath(scope, name, active.Base.ID))
		if err != nil {
			return value, "", "", "", fmt.Errorf("projection: read base: %w", err)
		}
		if int64(len(payload)) != active.Base.Bytes || digestBytes(payload) != active.Base.Digest || active.Base.ID != active.Base.Digest {
			return value, "", "", "", errors.New("projection: base digest mismatch")
		}
		if err := Decode(payload, &value); err != nil {
			return value, "", "", "", fmt.Errorf("projection: decode base: %w", err)
		}
		if store.options.ValidateBase != nil {
			if err := store.options.ValidateBase(value); err != nil {
				return value, "", "", "", fmt.Errorf("projection: corrupt base: %w", err)
			}
		}
		buildDigest = chainDigest("base", active.Base.Digest)
	}
	previousIdentity := manifestIdentity(Manifest{
		SchemaVersion: SchemaVersion, StorageVersion: StorageAlgorithmVersion,
		Projection: name, Scope: scope, Base: active.Base,
		Segments: []SegmentRef{}, BaseSourceDigest: baseSourceDigest(active),
		SourceDigest: baseSourceDigest(active),
		BuildDigest:  buildDigest, BaseLastApplied: active.BaseLastApplied,
		LastApplied: active.BaseLastApplied,
	})
	sourceDigest := baseSourceDigest(active)
	for index, ref := range active.Segments {
		if ref.PreviousIdentity != previousIdentity {
			return value, "", "", "", fmt.Errorf("projection: segment %d previous manifest identity mismatch", index)
		}
		payload, err := store.Workspace.Read(ctx, store.segmentPath(scope, name, ref.ID))
		if err != nil {
			return value, "", "", "", fmt.Errorf("projection: read segment %d: %w", index, err)
		}
		if int64(len(payload)) != ref.Bytes || digestBytes(payload) != ref.Digest {
			return value, "", "", "", fmt.Errorf("projection: segment %d digest mismatch", index)
		}
		var item segment[D]
		if err := Decode(payload, &item); err != nil {
			return value, "", "", "", fmt.Errorf("projection: decode segment %d: %w", index, err)
		}
		if item.SchemaVersion != SchemaVersion || item.StorageVersion != StorageAlgorithmVersion ||
			item.Projection != name || item.Scope != scope || item.ID != ref.ID ||
			item.PreviousIdentity != previousIdentity || item.SourceRevision != ref.SourceRevision ||
			item.DeltaDigest != ref.DeltaDigest || item.SourceInput != ref.SourceInput {
			return value, "", "", "", fmt.Errorf("projection: corrupt segment %d address or chain", index)
		}
		canonical, err := json.Marshal(item.Delta)
		if err != nil || digestBytes(canonical) != item.DeltaDigest {
			return value, "", "", "", fmt.Errorf("projection: segment %d delta digest mismatch", index)
		}
		sourceDigest = chainDigest("source", sourceDigest, item.SourceInput)
		buildDigest = chainDigest("build", buildDigest, item.ID)
		if item.SourceDigest != sourceDigest || item.BuildDigest != buildDigest {
			return value, "", "", "", fmt.Errorf("projection: segment %d cumulative digest mismatch", index)
		}
		if store.options.ValidateDelta != nil {
			if err := store.options.ValidateDelta(item.Delta); err != nil {
				return value, "", "", "", fmt.Errorf("projection: corrupt segment %d delta: %w", index, err)
			}
		}
		if err := store.options.Apply(&value, item.Delta); err != nil {
			return value, "", "", "", fmt.Errorf("projection: apply segment %d: %w", index, err)
		}
		prefix := active
		prefix.Segments = append([]SegmentRef(nil), active.Segments[:index+1]...)
		prefix.LastApplied = &ReplayRef{
			SourceRevision: ref.SourceRevision, DeltaDigest: ref.DeltaDigest, SourceInput: ref.SourceInput,
		}
		prefix.SourceDigest, prefix.BuildDigest, prefix.Identity = sourceDigest, buildDigest, ""
		previousIdentity = manifestIdentity(prefix)
	}
	if store.options.ValidateBase != nil {
		if err := store.options.ValidateBase(value); err != nil {
			return value, "", "", "", fmt.Errorf("projection: corrupt materialized state: %w", err)
		}
	}
	return value, sourceDigest, buildDigest, previousIdentity, nil
}

func (store *Store[B, D]) publishBaseLocked(
	ctx context.Context,
	scope sdkmemory.Scope,
	name string,
	payload []byte,
	sourceDigest string,
	lastApplied *ReplayRef,
) error {
	baseDigest := digestBytes(payload)
	if sourceDigest == "" {
		sourceDigest = baseDigest
	}
	if err := store.writeImmutable(ctx, store.buildPath(scope, name, baseDigest), payload); err != nil {
		return fmt.Errorf("projection: write immutable base: %w", err)
	}
	active := Manifest{
		SchemaVersion: SchemaVersion, StorageVersion: StorageAlgorithmVersion,
		Projection: name, Scope: scope,
		Base:     &BaseRef{ID: baseDigest, Digest: baseDigest, Bytes: int64(len(payload))},
		Segments: []SegmentRef{}, BaseSourceDigest: sourceDigest, SourceDigest: sourceDigest,
		BuildDigest:     chainDigest("base", baseDigest),
		BaseLastApplied: lastApplied, LastApplied: lastApplied,
	}
	active.Identity = manifestIdentity(active)
	if err := store.publishManifest(ctx, scope, name, active); err != nil {
		return err
	}
	store.cache = nil
	return nil
}

func (store *Store[B, D]) writeImmutable(ctx context.Context, name string, payload []byte) error {
	existing, err := store.Workspace.Read(ctx, name)
	if err == nil {
		if !bytes.Equal(existing, payload) {
			return errors.New("projection: immutable object conflict")
		}
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return err
	}
	return workspace.AtomicWrite(ctx, store.Workspace, name, append([]byte(nil), payload...))
}

func (store *Store[B, D]) publishManifest(ctx context.Context, scope sdkmemory.Scope, name string, active Manifest) error {
	payload, err := json.Marshal(active)
	if err != nil {
		return fmt.Errorf("projection: encode active manifest: %w", err)
	}
	if err := workspace.AtomicWrite(ctx, store.Workspace, store.activePath(scope, name), payload); err != nil {
		return fmt.Errorf("projection: publish active manifest: %w", err)
	}
	return nil
}

func (store *Store[B, D]) loadManifestLocked(ctx context.Context, scope sdkmemory.Scope, name string) (Manifest, bool, error) {
	active, found, err := store.loadManifestForAuditLocked(ctx, scope, name)
	if err != nil || !found {
		return active, found, err
	}
	if active.Identity == "" || active.Identity != manifestIdentity(active) {
		return Manifest{}, false, errors.New("projection: corrupt active manifest identity")
	}
	return active, true, nil
}

func (store *Store[B, D]) loadManifestForAuditLocked(
	ctx context.Context,
	scope sdkmemory.Scope,
	name string,
) (Manifest, bool, error) {
	data, err := store.Workspace.Read(ctx, store.activePath(scope, name))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Manifest{}, false, nil
		}
		return Manifest{}, false, fmt.Errorf("projection: read active manifest: %w", err)
	}
	var active Manifest
	if err := Decode(data, &active); err != nil {
		return Manifest{}, false, fmt.Errorf("projection: decode active manifest: %w", err)
	}
	if active.SchemaVersion != SchemaVersion || active.StorageVersion != StorageAlgorithmVersion ||
		active.Projection != name || active.Scope != scope || active.Identity == "" {
		return Manifest{}, false, errors.New("projection: corrupt active manifest address or schema")
	}
	if len(active.Segments) > store.options.Thresholds.MaxSegments {
		return Manifest{}, false, errors.New("projection: active manifest exceeds segment threshold")
	}
	if len(active.Segments) == 0 {
		if !reflect.DeepEqual(active.LastApplied, active.BaseLastApplied) {
			return Manifest{}, false, errors.New("projection: compacted manifest replay identity mismatch")
		}
	} else {
		last := active.Segments[len(active.Segments)-1]
		expected := &ReplayRef{
			SourceRevision: last.SourceRevision, DeltaDigest: last.DeltaDigest, SourceInput: last.SourceInput,
		}
		if !reflect.DeepEqual(active.LastApplied, expected) {
			return Manifest{}, false, errors.New("projection: active manifest last segment mismatch")
		}
	}
	return active, true, nil
}

func newManifest(scope sdkmemory.Scope, name string) Manifest {
	active := Manifest{
		SchemaVersion: SchemaVersion, StorageVersion: StorageAlgorithmVersion,
		Projection: name, Scope: scope, Segments: []SegmentRef{},
	}
	active.Identity = manifestIdentity(active)
	return active
}

func baseSourceDigest(active Manifest) string {
	return active.BaseSourceDigest
}

func manifestIdentity(active Manifest) string {
	active.Identity = ""
	data, _ := json.Marshal(active)
	return chainDigest("manifest", string(data))
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func chainDigest(domain string, values ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	for _, value := range values {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func cloneTyped[T any](value T) (T, error) {
	var cloned T
	data, err := json.Marshal(value)
	if err != nil {
		return cloned, fmt.Errorf("projection: clone encode: %w", err)
	}
	if err := Decode(data, &cloned); err != nil {
		return cloned, fmt.Errorf("projection: clone decode: %w", err)
	}
	return cloned, nil
}

type EntryDelta[E any] struct {
	Upserts            []E                         `json:"upserts,omitempty"`
	DeleteIDs          []string                    `json:"delete_ids,omitempty"`
	DeleteDocuments    []component.DocumentAddress `json:"delete_documents,omitempty"`
	ReconcileDocuments []component.DocumentAddress `json:"reconcile_documents,omitempty"`
	ActiveIDs          []string                    `json:"active_ids,omitempty"`
}

type EntryKey[E any] struct {
	ID       func(E) string
	Document func(E) component.DocumentAddress
}

func CanonicalEntryDelta[E any](delta EntryDelta[E], key EntryKey[E]) EntryDelta[E] {
	sort.Slice(delta.Upserts, func(i, j int) bool { return key.ID(delta.Upserts[i]) < key.ID(delta.Upserts[j]) })
	delta.DeleteIDs = canonicalStrings(delta.DeleteIDs)
	delta.DeleteDocuments = canonicalDocumentAddresses(delta.DeleteDocuments)
	delta.ReconcileDocuments = canonicalDocumentAddresses(delta.ReconcileDocuments)
	delta.ActiveIDs = canonicalStrings(delta.ActiveIDs)
	return delta
}

func ApplyEntryDelta[E any](base []E, delta EntryDelta[E], key EntryKey[E]) []E {
	values := make(map[string]E, len(base)+len(delta.Upserts))
	for _, value := range base {
		values[key.ID(value)] = value
	}
	for _, id := range delta.DeleteIDs {
		delete(values, id)
	}
	deletedDocuments := documentAddressSet(delta.DeleteDocuments)
	reconciledDocuments := documentAddressSet(delta.ReconcileDocuments)
	active := stringSet(delta.ActiveIDs)
	for id, value := range values {
		document := key.Document(value)
		if _, deleted := deletedDocuments[document.Key()]; deleted {
			delete(values, id)
			continue
		}
		if _, reconcile := reconciledDocuments[document.Key()]; reconcile {
			if _, keep := active[id]; !keep {
				delete(values, id)
			}
		}
	}
	for _, value := range delta.Upserts {
		values[key.ID(value)] = value
	}
	result := make([]E, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return key.ID(result[i]) < key.ID(result[j]) })
	return result
}

func canonicalDocumentAddresses(values []component.DocumentAddress) []component.DocumentAddress {
	result := append([]component.DocumentAddress(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i].Key() < result[j].Key() })
	write := 0
	for _, value := range result {
		if write > 0 && result[write-1] == value {
			continue
		}
		result[write] = value
		write++
	}
	return result[:write]
}

func documentAddressSet(values []component.DocumentAddress) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value.Key()] = struct{}{}
	}
	return result
}

func canonicalStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	write := 0
	for _, value := range result {
		if value == "" || (write > 0 && result[write-1] == value) {
			continue
		}
		result[write] = value
		write++
	}
	return result[:write]
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func (store *Store[B, D]) root(scope sdkmemory.Scope, name string) string {
	return path.Join("projections", store.Family, "v2", "partitions", encode(scope.RuntimeID),
		encode(scope.UserID), encode(scope.AgentID), "indexes", encode(name))
}

func (store *Store[B, D]) activePath(scope sdkmemory.Scope, name string) string {
	return path.Join(store.root(scope, name), "active.json")
}

func (store *Store[B, D]) buildPath(scope sdkmemory.Scope, name, build string) string {
	return path.Join(store.root(scope, name), "bases", encode(build)+".json")
}

func (store *Store[B, D]) segmentPath(scope sdkmemory.Scope, name, segment string) string {
	return path.Join(store.root(scope, name), "segments", encode(segment)+".json")
}

func validateAddress(scope sdkmemory.Scope, name string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return errors.New("projection: projection name is required")
	}
	return nil
}

func encode(value string) string {
	return "k_" + base64.RawURLEncoding.EncodeToString([]byte(value))
}

// Decode strictly decodes a schema document.
func Decode(data []byte, destination any) error {
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

// MatchesRequest applies the read-side conversation/dataset selectors encoded
// by the ContextProvider without weakening the hard Scope partition.
func MatchesRequest(metadata sdkmemory.Metadata, address component.CandidateAddress) (bool, error) {
	if conversationID := metadata["conversation_id"]; conversationID != "" {
		switch address.Kind {
		case sdkmemory.ContextRawMessage, sdkmemory.ContextFact:
			if address.ConversationID != conversationID {
				return false, nil
			}
		}
	}
	rawDatasets := metadata["dataset_ids"]
	if rawDatasets == "" || !isDocumentKind(address.Kind) {
		return true, nil
	}
	var datasetIDs []string
	if err := json.Unmarshal([]byte(rawDatasets), &datasetIDs); err != nil {
		return false, fmt.Errorf("projection: decode dataset_ids selector: %w", err)
	}
	for _, datasetID := range datasetIDs {
		if address.DatasetID == datasetID {
			return true, nil
		}
	}
	return false, nil
}

func isDocumentKind(kind sdkmemory.ContextItemKind) bool {
	switch kind {
	case sdkmemory.ContextDocumentResource, sdkmemory.ContextDocumentSection,
		sdkmemory.ContextDocumentChunk, sdkmemory.ContextDocumentSummary:
		return true
	default:
		return false
	}
}
