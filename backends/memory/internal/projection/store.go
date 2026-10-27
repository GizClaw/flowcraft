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

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	"github.com/GizClaw/flowcraft/core/errdefs"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const (
	SchemaVersion           = 3
	StorageAlgorithmVersion = "workspace-sharded-lsm-v1"
	DefaultMaxSegments      = 64
	DefaultMinDeltaBytes    = 4 << 20
	DefaultCompactionRatio  = 2
	// DefaultCacheBytes is the budget a store keeps decoded bases under when
	// its lane configures none: 2 GiB, which holds the working set of the
	// conversations one process serves -- a conversation's shard is a few
	// megabytes of table -- while staying far below the footprint of keeping
	// every table the process ever read.
	DefaultCacheBytes int64 = 2 << 30
)

// Address names one projection state: a hard scope and one shard within it.
//
// The scope is the tenant boundary nothing crosses; the shard is the unit a
// request reads and a delta writes, so two addresses of one scope are two
// independent states with their own manifest, base and segments. Every method
// that reads or writes state takes one, which is what keeps a write's cost and
// a read's cost proportional to one conversation or one dataset instead of to
// everything the scope ever indexed.
type Address struct {
	Scope corememory.Scope
	Shard string
}

// validate checks that the address names a state this store can reach: a
// complete scope and a shard with one of the two address families.
func (address Address) validate(name string) error {
	if err := validateAddress(address.Scope, name); err != nil {
		return err
	}
	_, err := ShardSegment(address.Shard)
	return err
}

// Thresholds decides when a delta is applied as a segment and when the state
// is rewritten as a new base.
//
// The byte trigger is relative to the base, because the fixed one it replaced
// was not: 4 MiB is 0.2% of a 2 GiB base, so a scope-sized lane rewrote its
// whole base every 32 commits and spent a run writing 2 GiB snapshots. With
// the default ratio, a 10 MB base is rewritten once its deltas reach 5 MB and
// a 2 GiB base at 1 GiB. MinDeltaBytes floors the trigger, so a small base is
// not rewritten on every commit, and MaxSegments caps the segment count that
// one manifest has to describe.
type Thresholds struct {
	MaxSegments   int   `json:"max_segments"`
	MinDeltaBytes int64 `json:"min_delta_bytes"`
	Ratio         int   `json:"ratio"`
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		MaxSegments: DefaultMaxSegments, MinDeltaBytes: DefaultMinDeltaBytes, Ratio: DefaultCompactionRatio,
	}
}

func (thresholds Thresholds) normalized() (Thresholds, error) {
	if thresholds.MaxSegments == 0 {
		thresholds.MaxSegments = DefaultMaxSegments
	}
	if thresholds.MinDeltaBytes == 0 {
		thresholds.MinDeltaBytes = DefaultMinDeltaBytes
	}
	if thresholds.Ratio == 0 {
		thresholds.Ratio = DefaultCompactionRatio
	}
	if thresholds.MaxSegments < 1 || thresholds.MinDeltaBytes < 1 || thresholds.Ratio < 1 {
		return Thresholds{}, errors.New("projection: compaction thresholds must be positive")
	}
	return thresholds, nil
}

// over reports whether one more segment of deltaBytes, carrying the segments
// already stored over a base of baseBytes, is the point to compact. The
// segment count is an upper bound that keeps a manifest readable; the byte
// trigger is what bounds write amplification on a growing base.
func (thresholds Thresholds) over(segments int, deltaBytes, baseBytes int64) bool {
	if segments > thresholds.MaxSegments {
		return true
	}
	limit := thresholds.MinDeltaBytes
	if scaled := baseBytes / int64(thresholds.Ratio); scaled > limit {
		limit = scaled
	}
	return deltaBytes > limit
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
//
// ProducerVersion records which producer wrote the state the manifest points
// at. It is stamped when the manifest is created and an ordinary delta apply
// leaves it alone: the stamp describes the state as a whole, so the entries a
// newer producer appends do not make an older state current -- they leave it
// mixed, and the stamp is what reports that a rebuild would converge it. A
// rebuild (FullRebuild) publishes state the running producer wrote, so it
// stamps its own version.
//
// The stamp is metadata about the writer rather than part of the state, so it
// is deliberately left out of Identity (see manifestIdentity): a reader that
// predates the field recomputes the same identity, which is what lets a build
// without the stamp keep reading manifests one with it wrote.
type Manifest struct {
	SchemaVersion   int              `json:"schema_version"`
	StorageVersion  string           `json:"storage_version"`
	ProducerVersion string           `json:"producer_version,omitempty"`
	Projection      string           `json:"projection"`
	Scope           corememory.Scope `json:"scope"`
	// Shard names the state this manifest points at. It is part of the
	// manifest's address and of its identity: a manifest published under one
	// shard's directory that names another shard is not that shard's state,
	// and reading it as such would serve one conversation's records to
	// another's selector.
	Shard            string       `json:"shard"`
	Base             *BaseRef     `json:"base,omitempty"`
	Segments         []SegmentRef `json:"segments"`
	BaseSourceDigest string       `json:"base_source_digest"`
	SourceDigest     string       `json:"source_digest"`
	BuildDigest      string       `json:"build_digest"`
	BaseLastApplied  *ReplayRef   `json:"base_last_applied,omitempty"`
	LastApplied      *ReplayRef   `json:"last_applied,omitempty"`
	Identity         string       `json:"identity"`
}

type DigestEvidence struct {
	StoredSourceDigest   string
	ComputedSourceDigest string
	StoredBuildDigest    string
	ComputedBuildDigest  string
}

type segment[D any] struct {
	SchemaVersion    int              `json:"schema_version"`
	StorageVersion   string           `json:"storage_version"`
	Projection       string           `json:"projection"`
	Scope            corememory.Scope `json:"scope"`
	Shard            string           `json:"shard"`
	ID               string           `json:"id"`
	PreviousIdentity string           `json:"previous_identity"`
	SourceRevision   string           `json:"source_revision,omitempty"`
	DeltaDigest      string           `json:"delta_digest"`
	SourceInput      string           `json:"source_input"`
	SourceDigest     string           `json:"source_digest"`
	BuildDigest      string           `json:"build_digest"`
	Delta            D                `json:"delta"`
}

type TypedOptions[B, D any] struct {
	Thresholds Thresholds
	// ProducerVersion names the identity of the state this store writes: the
	// lane's algorithm plus the parameters that shape a persisted record. It
	// is stamped into every manifest the store creates, and a reader compares
	// it against the version the running producer writes with.
	ProducerVersion string
	// ResidentBases bounds how many partitions' bases the store keeps decoded.
	// Zero keeps one, which is what a store without the bound kept; a partition
	// the store does not hold is decoded again when it is next read.
	ResidentBases int
	// MaxBytes bounds the bytes of resident bases, weighed by BaseBytes. It is
	// what a store of many partitions is bounded by: a count bound is a budget
	// only while every base is the same size, and one conversation's table is
	// not another's. Zero keeps DefaultCacheBytes when the store weighs its
	// bases, and leaves the bound to ResidentBases when it does not.
	MaxBytes int64
	// BaseBytes weighs one decoded base for the byte bound. A lane that cannot
	// say what a base costs -- or one that says zero -- is bounded by count.
	BaseBytes     func(B) int64
	Canonicalize  func(D) D
	ValidateBase  func(B) error
	ValidateDelta func(D) error
	Apply         func(*B, D) error
}

// CacheStats is what one store keeps decoded: the bases it holds, the bytes
// they are reckoned at, and what the reads did with them.
//
// A hit is a read served out of a base already materialized, a miss is one that
// had to decode the active state off storage, and an uncached base is one read
// and dropped because it alone exceeds the budget -- scanned, not held. The
// counters describe the process that reports them: they start at zero and only
// grow, so a caller reading a run subtracts two readings.
type CacheStats struct {
	Bases     int   `json:"bases"`
	Bytes     int64 `json:"bytes"`
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	Evictions int64 `json:"evictions"`
	Uncached  int64 `json:"uncached"`
}

// residentBase is one partition's materialized base, kept decoded.
//
// Every reader of a lane asks the store for the same base: the write path asks
// which entries it already holds, a query asks for the table it scans, and a
// warm-up asks for the table the next query will read. Decoding the base once
// and keeping it turned those asks into a map lookup.
//
// The value belongs to the store. Only the store writes it, only under its
// mutex, and only by replacing the slices it holds -- never by writing through
// them -- so a caller that took a copy of the struct keeps reading the state it
// was handed while a later delta replaces the store's copy.
type residentBase[B any] struct {
	identity string
	value    B
	// bytes is what the base was weighed at when it was kept, which is what
	// the budget counts. Weighting it again on every eviction would cost more
	// than the memory it saves, and a base the store advances is re-weighed
	// where it moves.
	bytes int64
	// used is the store's clock reading at the base's last read. The bound
	// evicts the base read least recently, which is the one the caller is
	// least likely to read again.
	used int64
}

// Store owns the common typed immutable-base and ordered-segment protocol.
//
// One store serves every shard of one family: the shards of a scope share the
// protocol and the cache, not the state. Each shard is locked on its own, so a
// compaction of one conversation's table no longer serializes the deltas of the
// thirty-one others -- the property a scope-sized lock had taken away.
type Store[B, D any] struct {
	KV      storage.Store
	Family  string
	options TypedOptions[B, D]
	// locks holds one mutex per address. A shard's state is written by one
	// holder at a time; two shards are written at the same time.
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
	// cacheMu guards the resident bases and their recency clock, which is
	// shared by every shard: an eviction is a decision about the cache, not
	// about one address.
	cacheMu sync.Mutex
	bases   map[string]*residentBase[B]
	clock   int64
	// residentBytes is the sum of the kept bases' weights, maintained as they
	// are kept, advanced and dropped so the budget never recounts the cache.
	residentBytes  int64
	cacheHits      int64
	cacheMisses    int64
	cacheEvictions int64
	cacheUncached  int64
}

type ApplyResult struct {
	Manifest  Manifest
	Replayed  bool
	Compacted bool
}

func NewTypedStore[B, D any](kv storage.Store, family string, options TypedOptions[B, D]) (*Store[B, D], error) {
	if nilInterface(kv) {
		return nil, errors.New("projection: store is required")
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
	if options.MaxBytes < 0 {
		return nil, errors.New("projection: cache budget must not be negative")
	}
	if options.MaxBytes == 0 && options.BaseBytes != nil {
		options.MaxBytes = DefaultCacheBytes
	}
	options.Thresholds = thresholds
	if _, ok := kv.(storage.PutIfAbsentStore); !ok {
		return nil, errors.New("projection: store must support immutable writes")
	}
	return &Store[B, D]{KV: kv, Family: family, options: options,
		locks: make(map[string]*sync.Mutex), bases: make(map[string]*residentBase[B])}, nil
}

// lock returns the mutex one address's state is written under, creating it on
// first use. The map is small -- one entry per address a process has touched --
// and never drops an entry: a lock is cheap and a released one that a
// concurrent writer still holds must not be replaced by a fresh mutex.
func (store *Store[B, D]) lock(address Address, name string) *sync.Mutex {
	key := store.residentKey(address, name)
	store.locksMu.Lock()
	defer store.locksMu.Unlock()
	mutex, ok := store.locks[key]
	if !ok {
		mutex = &sync.Mutex{}
		store.locks[key] = mutex
	}
	return mutex
}

func (store *Store[B, D]) FullRebuild(ctx context.Context, address Address, name string, base B, sourceDigest string) error {
	if err := address.validate(name); err != nil {
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
	mutex := store.lock(address, name)
	mutex.Lock()
	defer mutex.Unlock()
	// A rebuild publishes state this producer wrote, so the manifest it leaves
	// behind names this producer rather than the one it replaced.
	if err := store.publishBaseLocked(ctx, address, name, payload, sourceDigest, store.options.ProducerVersion, nil); err != nil {
		return err
	}
	// The published base is not the one the store had decoded, and the value
	// this call marshalled belongs to the caller: the next reader decodes it,
	// and the warm-up a lane defers after a rebuild is that reader.
	store.forgetLocked(store.residentKey(address, name))
	return nil
}

func (store *Store[B, D]) ApplyDelta(
	ctx context.Context,
	address Address,
	name string,
	delta D,
	sourceRevision string,
	sourceInput string,
) (ApplyResult, error) {
	if err := address.validate(name); err != nil {
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

	mutex := store.lock(address, name)
	mutex.Lock()
	defer mutex.Unlock()
	active, found, err := store.loadManifestLocked(ctx, address, name)
	if err != nil {
		return ApplyResult{}, err
	}
	if !found {
		active = store.newManifest(address, name)
	}
	// What the delta this call publishes supersedes: a resident base of this
	// identity is the state the delta was applied to, so it can be moved
	// forward by the delta instead of decoded again.
	previousIdentity := active.Identity
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
		Projection: name, Scope: address.Scope, Shard: address.Shard,
		ID: segmentID, PreviousIdentity: active.Identity,
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
	var baseBytes int64
	if active.Base != nil {
		baseBytes = active.Base.Bytes
	}
	if store.options.Thresholds.over(len(active.Segments)+1, totalBytes, baseBytes) {
		base, err := store.materializeLocked(ctx, address, name, active)
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
		// Compaction rewrites the storage shape, not the state: the producer
		// that wrote the entries stays the producer the manifest names.
		if err := store.publishBaseLocked(ctx, address, name, payload, nextSource, active.ProducerVersion, replay); err != nil {
			return ApplyResult{}, err
		}
		compacted, _, err := store.loadManifestLocked(ctx, address, name)
		// The base the compaction marshalled is the state the manifest now
		// points at, and it is the store's own copy -- materializeLocked
		// returned one -- so it becomes the resident base without a decode.
		if err == nil {
			store.keepLocked(store.residentKey(address, name), compacted.Identity, base)
		}
		return ApplyResult{Manifest: compacted, Compacted: true}, err
	}
	if err := store.writeImmutable(ctx, store.segmentPath(address, name, segmentID), segmentPayload); err != nil {
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
	if err := store.publishManifest(ctx, address, name, active); err != nil {
		return ApplyResult{}, err
	}
	store.advanceLocked(store.residentKey(address, name), previousIdentity, active.Identity, delta)
	return ApplyResult{Manifest: active}, nil
}

func (store *Store[B, D]) Materialize(ctx context.Context, address Address, name string) (B, Manifest, error) {
	var zero B
	if err := address.validate(name); err != nil {
		return zero, Manifest{}, err
	}
	mutex := store.lock(address, name)
	mutex.Lock()
	defer mutex.Unlock()
	active, found, err := store.loadManifestLocked(ctx, address, name)
	if err != nil {
		// The manifest area itself did not read back. That answers the same
		// question as state that cannot be materialized -- something is
		// stored here and it cannot be served -- so it is reported the same
		// way (see StateError).
		return zero, Manifest{}, &component.StateError{Projection: name, Err: err}
	}
	if !found {
		return zero, Manifest{}, fmt.Errorf("projection: read active manifest: %w", storage.ErrNotFound)
	}
	base, err := store.materializeLocked(ctx, address, name, active)
	if err != nil {
		// The manifest is there and the state it points at is not readable.
		// A reader has to be able to tell this from a lane that holds
		// nothing -- that one reports storage.ErrNotFound above -- because
		// the two answer different questions: "nothing was ever written
		// here" against "what was written cannot be served".
		return base, active, &component.StateError{Projection: name, Err: err}
	}
	return base, active, nil
}

// MaterializeView materializes a scope's projection for reading, sharing the
// store's resident base rather than copying it.
//
// What it returns is read-only: the value shares its slices with the store and
// with every other reader of the same state, and the store replaces those
// slices -- it does not rewrite them -- when a later delta moves the state
// forward, so a caller that keeps reading what it was handed keeps reading one
// consistent state. A caller that needs to apply deltas to a base reads it with
// Materialize, which returns a copy of its own.
//
// The distinction is what a write path needs: a delta's apply reads the entries
// the base already holds, to reuse the ones whose content did not change, and on
// a scope-sized table the copy costs more than everything else the write does.
// Measured on the eval store, materializing one 9324-entry, 2048-dimension table
// took 14.0s, and a derivation applies one delta per commit over hundreds of
// commits.
func (store *Store[B, D]) MaterializeView(ctx context.Context, address Address, name string) (B, Manifest, error) {
	var zero B
	if err := address.validate(name); err != nil {
		return zero, Manifest{}, err
	}
	mutex := store.lock(address, name)
	mutex.Lock()
	defer mutex.Unlock()
	active, found, err := store.loadManifestLocked(ctx, address, name)
	if err != nil {
		return zero, Manifest{}, &component.StateError{Projection: name, Err: err}
	}
	if !found {
		return zero, Manifest{}, fmt.Errorf("projection: read active manifest: %w", storage.ErrNotFound)
	}
	base, err := store.baseLocked(ctx, address, name, active)
	if err != nil {
		return base, active, &component.StateError{Projection: name, Err: err}
	}
	return base, active, nil
}

func (store *Store[B, D]) Audit(ctx context.Context, address Address, name string) (Manifest, bool, error) {
	if err := address.validate(name); err != nil {
		return Manifest{}, false, err
	}
	mutex := store.lock(address, name)
	mutex.Lock()
	defer mutex.Unlock()
	active, found, err := store.loadManifestLocked(ctx, address, name)
	if err != nil || !found {
		return Manifest{}, found, err
	}
	_, err = store.materializeUncachedLocked(ctx, address, name, active)
	return active, true, err
}

// AuditDigestEvidence audits every shard of one scope under one name and
// reports the scope's root evidence: the digests the shards' manifests carry,
// and the digests recomputing their bytes produces. A scope has one answer
// because a host asks one question -- whether the projection it reads matches
// what it can rebuild -- and the answer has to cover every shard, or a corrupt
// shard nobody queried would stay invisible.
//
// found is false when no shard holds state, which is the state of a scope that
// was never indexed.
func (store *Store[B, D]) AuditDigestEvidence(
	ctx context.Context,
	scope corememory.Scope,
	name string,
) (DigestEvidence, bool, error) {
	shards, err := store.Shards(ctx, scope, name)
	if err != nil {
		return DigestEvidence{}, false, err
	}
	var storedSource, computedSource, storedBuild, computedBuild []string
	for _, shard := range shards {
		address := Address{Scope: scope, Shard: shard}
		mutex := store.lock(address, name)
		mutex.Lock()
		active, found, err := store.loadManifestForAuditLocked(ctx, address, name)
		if err != nil {
			mutex.Unlock()
			return DigestEvidence{}, false, err
		}
		if !found {
			// A shard directory without a manifest is a publication that
			// failed before it pointed at anything: there is no state to
			// audit, and no state a reader serves.
			mutex.Unlock()
			continue
		}
		_, sourceDigest, buildDigest, identity, err := store.materializeAuditLocked(ctx, address, name, active)
		mutex.Unlock()
		if err != nil {
			return DigestEvidence{}, true, err
		}
		if sourceDigest == active.SourceDigest && buildDigest == active.BuildDigest && identity != active.Identity {
			return DigestEvidence{}, true, errors.New("projection: active manifest identity mismatch")
		}
		storedSource = append(storedSource, chainDigest("shard", shard, active.SourceDigest))
		storedBuild = append(storedBuild, chainDigest("shard", shard, active.BuildDigest))
		computedSource = append(computedSource, chainDigest("shard", shard, sourceDigest))
		computedBuild = append(computedBuild, chainDigest("shard", shard, buildDigest))
	}
	if len(storedSource) == 0 {
		return DigestEvidence{}, false, nil
	}
	return DigestEvidence{
		StoredSourceDigest:   rootDigest(storedSource),
		ComputedSourceDigest: rootDigest(computedSource),
		StoredBuildDigest:    rootDigest(storedBuild),
		ComputedBuildDigest:  rootDigest(computedBuild),
	}, true, nil
}

// ProducerIdentity reports the identity of one projection's persisted state:
// the producer version stamped into its shards' manifests, together with the
// root digests of their cumulative state. It is a bounded read -- it decodes
// each active manifest and reads nothing else -- so a host can ask whether a
// lane is stale without materializing it.
//
// The version is reported only when every shard agrees on it. A scope whose
// shards were written by different producers has no single version, and
// reporting one of them would claim the state is more converged than it is:
// Mixed is set and Version stays empty, which fails every "is this current"
// comparison exactly as the mismatch it is.
func (store *Store[B, D]) ProducerIdentity(
	ctx context.Context,
	scope corememory.Scope,
	name string,
) (component.ProducerIdentity, bool, error) {
	shards, err := store.Shards(ctx, scope, name)
	if err != nil {
		return component.ProducerIdentity{}, false, err
	}
	var versions, sources, builds []string
	for _, shard := range shards {
		address := Address{Scope: scope, Shard: shard}
		mutex := store.lock(address, name)
		mutex.Lock()
		active, found, err := store.loadManifestLocked(ctx, address, name)
		mutex.Unlock()
		if err != nil {
			return component.ProducerIdentity{}, false, err
		}
		if !found {
			continue
		}
		versions = append(versions, active.ProducerVersion)
		sources = append(sources, chainDigest("shard", shard, active.SourceDigest))
		builds = append(builds, chainDigest("shard", shard, active.BuildDigest))
	}
	if len(sources) == 0 {
		return component.ProducerIdentity{}, false, nil
	}
	identity := component.ProducerIdentity{
		Version:      versions[0],
		SourceDigest: rootDigest(sources),
		BuildDigest:  rootDigest(builds),
		Shards:       len(sources),
	}
	for _, version := range versions[1:] {
		if version != identity.Version {
			identity.Version, identity.Mixed = "", true
			break
		}
	}
	return identity, true, nil
}

// rootDigest folds the per-shard digests of one scope into the single digest a
// host compares. The entries are already ordered by shard and already carry the
// shard name, so two scopes with the same state under different addresses --
// or the same shards in a different order -- cannot fold to the same root.
func rootDigest(entries []string) string {
	return chainDigest("shards", entries...)
}

func (store *Store[B, D]) materializeLocked(ctx context.Context, address Address, name string, active Manifest) (B, error) {
	value, err := store.baseLocked(ctx, address, name, active)
	if err != nil {
		var zero B
		return zero, err
	}
	return cloneTyped(value)
}

// baseLocked returns the base for reading only: the store's resident copy when
// it holds this identity, a fresh decode that then becomes resident otherwise.
// Nothing is copied, and what it returns must not be written through.
func (store *Store[B, D]) baseLocked(
	ctx context.Context,
	address Address,
	name string,
	active Manifest,
) (B, error) {
	key := store.residentKey(address, name)
	if value, ok := store.residentLocked(key, active.Identity); ok {
		return value, nil
	}
	value, err := store.materializeUncachedLocked(ctx, address, name, active)
	if err != nil {
		var zero B
		return zero, err
	}
	store.cacheMu.Lock()
	store.cacheMisses++
	store.cacheMu.Unlock()
	store.keepLocked(key, active.Identity, value)
	return value, nil
}

// residentKey names the state one shard's projection is kept under: the
// projection, the partition the state lives in, and the shard within it.
func (store *Store[B, D]) residentKey(address Address, name string) string {
	return name + "\x00" + address.Scope.HardPartitionKey() + "\x00" + address.Shard
}

// residentLocked returns the base the store keeps for a key, when it holds the
// state the caller asked for. A base of another identity is dropped: the
// manifest is what names the active state, and what this store decoded for an
// earlier one is not it.
func (store *Store[B, D]) residentLocked(key, identity string) (B, bool) {
	var zero B
	store.cacheMu.Lock()
	defer store.cacheMu.Unlock()
	entry, ok := store.bases[key]
	if !ok {
		return zero, false
	}
	if entry.identity != identity {
		store.dropLocked(key)
		return zero, false
	}
	store.clock++
	entry.used = store.clock
	store.cacheHits++
	return entry.value, true
}

// keepLocked installs a base as the one the store keeps for a key. The value
// must be the store's to keep: a fresh decode, or a copy the store made for
// itself -- never a value a caller still holds.
//
// What makes room is the base read least recently, dropped until the cache is
// within its bound -- by bytes when the lane weighs its bases, else by count. A
// base that alone exceeds the byte budget is not kept at all: keeping it would
// evict every other base and still overshoot, so it is read, scanned, and
// dropped. What a bound costs is a decode of the base it evicted, never a
// different answer: what a reader is handed is the active state either way.
func (store *Store[B, D]) keepLocked(key, identity string, value B) {
	weight := store.weigh(value)
	store.cacheMu.Lock()
	defer store.cacheMu.Unlock()
	if store.bases == nil {
		store.bases = make(map[string]*residentBase[B])
	}
	store.dropLocked(key)
	store.clock++
	if limit := store.options.MaxBytes; limit > 0 && weight > limit {
		store.cacheUncached++
		return
	}
	for store.overBudgetLocked(weight) {
		oldest, used := "", int64(0)
		for candidate, entry := range store.bases {
			if oldest == "" || entry.used < used {
				oldest, used = candidate, entry.used
			}
		}
		if oldest == "" {
			break
		}
		store.dropLocked(oldest)
		store.cacheEvictions++
	}
	store.bases[key] = &residentBase[B]{identity: identity, value: value, bytes: weight, used: store.clock}
	store.residentBytes += weight
}

// overBudgetLocked reports whether keeping one more base of the given weight
// would put the cache over its bound. A budget of bytes is what a store of
// unevenly sized partitions is bounded by; the count bound is what a store of
// bases the lane cannot weigh is left with, and it keeps at least one.
func (store *Store[B, D]) overBudgetLocked(incoming int64) bool {
	if limit := store.options.MaxBytes; limit > 0 && store.options.BaseBytes != nil {
		return store.residentBytes+incoming > limit && len(store.bases) > 0
	}
	count := store.options.ResidentBases
	if count < 1 {
		count = 1
	}
	return len(store.bases) >= count
}

// weigh is what one base counts against the byte budget: the lane's own
// reading of it, and zero when the lane gives none.
func (store *Store[B, D]) weigh(value B) int64 {
	if store.options.BaseBytes == nil {
		return 0
	}
	return store.options.BaseBytes(value)
}

// dropLocked removes whatever the store keeps for a key and reports whether it
// held anything.
func (store *Store[B, D]) dropLocked(key string) bool {
	entry, ok := store.bases[key]
	if !ok {
		return false
	}
	delete(store.bases, key)
	store.residentBytes -= entry.bytes
	return true
}

// forgetLocked drops whatever the store keeps for a key.
func (store *Store[B, D]) forgetLocked(key string) {
	store.cacheMu.Lock()
	defer store.cacheMu.Unlock()
	store.dropLocked(key)
}

// Resident reports what the store keeps decoded: the bases, the bytes they are
// reckoned at, and what the reads did with them. It reads the counters under
// the cache lock and returns a value of its own, so a caller's reading cannot
// be changed by a read that arrives after it.
func (store *Store[B, D]) Resident() CacheStats {
	store.cacheMu.Lock()
	defer store.cacheMu.Unlock()
	return CacheStats{
		Bases:     len(store.bases),
		Bytes:     store.residentBytes,
		Hits:      store.cacheHits,
		Misses:    store.cacheMisses,
		Evictions: store.cacheEvictions,
		Uncached:  store.cacheUncached,
	}
}

// advanceLocked moves the resident base one delta forward instead of dropping
// it. The base the store keeps is the state the delta was applied to -- the
// identity the segment just published names as its previous identity -- so
// applying the same delta to it leaves it describing the state the new manifest
// publishes, and the next reader does not decode the base and replay the
// segment to find that out.
//
// The delta was validated before it was written, so an error here is a base
// that cannot be advanced: it is dropped rather than left half-moved, and the
// next read decodes the published state.
func (store *Store[B, D]) advanceLocked(key, previousIdentity, identity string, delta D) {
	store.cacheMu.Lock()
	defer store.cacheMu.Unlock()
	entry, ok := store.bases[key]
	if !ok {
		return
	}
	if entry.identity != previousIdentity {
		store.dropLocked(key)
		return
	}
	if err := store.options.Apply(&entry.value, delta); err != nil {
		store.dropLocked(key)
		return
	}
	store.clock++
	entry.identity, entry.used = identity, store.clock
	// The delta replaced some entries, so the base is not the one that was
	// weighed: the budget counts the weight it has now.
	store.residentBytes -= entry.bytes
	entry.bytes = store.weigh(entry.value)
	store.residentBytes += entry.bytes
}

func (store *Store[B, D]) materializeUncachedLocked(ctx context.Context, address Address, name string, active Manifest) (B, error) {
	value, sourceDigest, buildDigest, identity, err := store.materializeAuditLocked(ctx, address, name, active)
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
	address Address,
	name string,
	active Manifest,
) (B, string, string, string, error) {
	var value B
	buildDigest := ""
	if active.Base != nil {
		payload, err := store.KV.Get(ctx, store.buildPath(address, name, active.Base.ID))
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
		ProducerVersion: active.ProducerVersion,
		Projection:      name, Scope: address.Scope, Shard: address.Shard, Base: active.Base,
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
		payload, err := store.KV.Get(ctx, store.segmentPath(address, name, ref.ID))
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
			item.Projection != name || item.Scope != address.Scope || item.Shard != address.Shard ||
			item.ID != ref.ID ||
			item.PreviousIdentity != previousIdentity || item.SourceRevision != ref.SourceRevision ||
			item.DeltaDigest != ref.DeltaDigest || item.SourceInput != ref.SourceInput {
			return value, "", "", "", fmt.Errorf("projection: corrupt segment %d address or chain", index)
		}
		canonical, err := json.Marshal(item.Delta)
		if err != nil {
			return value, "", "", "", fmt.Errorf("projection: segment %d delta does not encode: %w", index, err)
		}
		if digestBytes(canonical) != item.DeltaDigest {
			// Both digests are named: a mismatch here is a condition whose
			// cause is otherwise invisible -- the stored bytes pass the
			// segment's own digest check and decode cleanly, so what differs
			// is what the decoded delta encodes back to, and the digests are
			// what a host can compare against a rebuilt lane.
			return value, "", "", "", fmt.Errorf(
				"projection: segment %d delta digest mismatch: stored %s, recomputed %s over %d bytes",
				index, item.DeltaDigest, digestBytes(canonical), len(canonical))
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
	address Address,
	name string,
	payload []byte,
	sourceDigest string,
	producerVersion string,
	lastApplied *ReplayRef,
) error {
	baseDigest := digestBytes(payload)
	if sourceDigest == "" {
		sourceDigest = baseDigest
	}
	// What this publication replaces is what the sweep spares. The read is
	// best-effort: a lane whose manifest cannot be decoded is a lane a rebuild
	// replaces, and refusing to publish because the state being replaced is
	// unreadable would be a repair that cannot repair.
	previous, previousFound, _ := store.loadManifestLocked(ctx, address, name)
	if err := store.writeImmutable(ctx, store.buildPath(address, name, baseDigest), payload); err != nil {
		return fmt.Errorf("projection: write immutable base: %w", err)
	}
	active := Manifest{
		SchemaVersion: SchemaVersion, StorageVersion: StorageAlgorithmVersion,
		ProducerVersion: producerVersion,
		Projection:      name, Scope: address.Scope, Shard: address.Shard,
		Base:     &BaseRef{ID: baseDigest, Digest: baseDigest, Bytes: int64(len(payload))},
		Segments: []SegmentRef{}, BaseSourceDigest: sourceDigest, SourceDigest: sourceDigest,
		BuildDigest:     chainDigest("base", baseDigest),
		BaseLastApplied: lastApplied, LastApplied: lastApplied,
	}
	active.Identity = manifestIdentity(active)
	if err := store.publishManifest(ctx, address, name, active); err != nil {
		return err
	}
	// The manifest is what makes state reachable, so the sweep runs after it is
	// durable: everything the new manifest does not name stopped being
	// readable at that instant, and the one generation of grace is what a
	// reader that resolved the previous manifest finishes on.
	store.sweepSupersededLocked(ctx, address, name, active, previous, previousFound)
	return nil
}

func (store *Store[B, D]) writeImmutable(ctx context.Context, name string, payload []byte) error {
	put, ok := store.KV.(storage.PutIfAbsentStore)
	if !ok {
		return errors.New("projection: store must support immutable writes")
	}
	data := append([]byte(nil), payload...)
	written, err := put.PutIfAbsent(ctx, name, data)
	if err != nil {
		return err
	}
	if written {
		return nil
	}
	existing, err := store.KV.Get(ctx, name)
	if err != nil {
		return err
	}
	if !bytes.Equal(existing, data) {
		return errors.New("projection: immutable object conflict")
	}
	return nil
}

func (store *Store[B, D]) publishManifest(ctx context.Context, address Address, name string, active Manifest) error {
	payload, err := json.Marshal(active)
	if err != nil {
		return fmt.Errorf("projection: encode active manifest: %w", err)
	}
	if err := store.KV.Put(ctx, store.activePath(address, name), payload); err != nil {
		return fmt.Errorf("projection: publish active manifest: %w", err)
	}
	return nil
}

func (store *Store[B, D]) loadManifestLocked(ctx context.Context, address Address, name string) (Manifest, bool, error) {
	active, found, err := store.loadManifestForAuditLocked(ctx, address, name)
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
	address Address,
	name string,
) (Manifest, bool, error) {
	data, err := store.KV.Get(ctx, store.activePath(address, name))
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
		active.Projection != name || active.Scope != address.Scope || active.Shard != address.Shard ||
		active.Identity == "" {
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

// newManifest is the manifest of a projection that holds nothing yet. It is
// the one place a manifest is stamped: every later write of the same state --
// an appended segment, a compaction -- carries the version forward.
func (store *Store[B, D]) newManifest(address Address, name string) Manifest {
	active := Manifest{
		SchemaVersion: SchemaVersion, StorageVersion: StorageAlgorithmVersion,
		ProducerVersion: store.options.ProducerVersion,
		Projection:      name, Scope: address.Scope, Shard: address.Shard, Segments: []SegmentRef{},
	}
	active.Identity = manifestIdentity(active)
	return active
}

func baseSourceDigest(active Manifest) string {
	return active.BaseSourceDigest
}

// manifestIdentity is the digest of what a manifest points at: every field
// that describes the stored state. Identity and the producer stamp are left
// out. Identity is the digest itself, and the stamp records who wrote the
// state rather than what the state is, so a reader that does not know the
// stamp recomputes the same identity as one that does -- a rollback to such a
// reader keeps reading the manifests this build wrote instead of reporting
// them corrupt. Nothing goes unchecked: the stamp is read and compared on its
// own (ProducerIdentity, verify, Diagnostics).
//
// The stamp is zeroed rather than skipped in the encoding, and the field
// marshals with omitempty, so the encoded bytes match what a reader without
// the field produces.
func manifestIdentity(active Manifest) string {
	active.Identity = ""
	active.ProducerVersion = ""
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
	// Copy before sorting: the delta arrives by value but its slices share
	// the caller's backing arrays, and canonicalization must not mutate
	// caller-owned data.
	upserts := append([]E(nil), delta.Upserts...)
	sort.Slice(upserts, func(i, j int) bool { return key.ID(upserts[i]) < key.ID(upserts[j]) })
	delta.Upserts = upserts
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

func (store *Store[B, D]) root(scope corememory.Scope, name string) string {
	return path.Join(store.indexesPrefix(scope), encode(name))
}

// shardRoot is the area one shard of one projection lives under. The address is
// validated by every entry point before it reaches here, so the segment cannot
// be missing: an unparsable shard was refused before any storage was touched.
func (store *Store[B, D]) shardRoot(address Address, name string) string {
	segment, _ := ShardSegment(address.Shard)
	return path.Join(store.root(address.Scope, name), "shards", segment)
}

// indexesPrefix is the area every index name of one family and scope lives
// under, the parent of one lane's root.
func (store *Store[B, D]) indexesPrefix(scope corememory.Scope) string {
	return path.Join("projections", store.Family, "v3", "partitions", encode(scope.RuntimeID),
		encode(scope.UserID), encode(scope.AgentID), "indexes")
}

func (store *Store[B, D]) activePath(address Address, name string) string {
	return path.Join(store.shardRoot(address, name), "active.json")
}

func (store *Store[B, D]) buildPath(address Address, name, build string) string {
	return path.Join(store.shardRoot(address, name), "bases", encode(build)+".json")
}

func (store *Store[B, D]) segmentPath(address Address, name, segment string) string {
	return path.Join(store.shardRoot(address, name), "segments", encode(segment)+".json")
}

func validateAddress(scope corememory.Scope, name string) error {
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
