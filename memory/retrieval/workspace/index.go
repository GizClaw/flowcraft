package workspace

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// Defaults applied when the corresponding [Option] is not supplied.
// Tuned for a few-thousand-doc namespace running on LocalWorkspace;
// callers with very different shapes (mostly-read, very-write-heavy,
// large vectors) should pass explicit options.
const (
	// DefaultMemtableMaxDocs is the doc-count threshold that
	// triggers a memtable flush. Mid-thousands keeps each segment
	// readable in a single Workspace.Read call.
	DefaultMemtableMaxDocs = 4096

	// DefaultMemtableMaxBytes is a soft budget on the JSON-encoded
	// size of staged docs. Whichever threshold (count or bytes) is
	// hit first triggers a flush.
	DefaultMemtableMaxBytes = 16 * 1024 * 1024 // 16 MiB

	// DefaultWALMaxBytes caps a single wal/<seq>.log file. When the
	// active log crosses this it is rotated and a fresh log is
	// opened; both logs are replayed on recovery.
	DefaultWALMaxBytes = 8 * 1024 * 1024 // 8 MiB

	// DefaultLockHeartbeat is how often the writer refreshes its
	// lockfile mtime. Stale lockfiles older than 3× this value are
	// considered abandoned and overridden on Open.
	DefaultLockHeartbeat = 5 * time.Second

	// DefaultCompactionInterval is the period of the background
	// size-tiered compactor's wake-up tick. Flushes also poke the
	// compactor inline so the worst-case lag between a flush
	// crossing a merge threshold and the merge starting is min(
	// DefaultCompactionInterval, one tick after that flush).
	DefaultCompactionInterval = 5 * time.Second

	// DefaultCompactionMinSegments is the smallest number of
	// segments in one size bucket that triggers a merge. Picking
	// 4 follows the LSM literature's "amplification vs read-side
	// reader fan-out" sweet spot for small indices; large
	// deployments may bump this.
	DefaultCompactionMinSegments = 4

	// DefaultCompactionMaxSize caps the per-segment size at which
	// further compaction stops. Beyond this, merging would produce
	// a single huge segment whose write amplification dominates
	// the gain in read fan-out.
	DefaultCompactionMaxSize = 256 * 1024 * 1024 // 256 MiB
)

// Config bundles the resolved configuration of an [Index]. It is
// produced from a slice of [Option] values; callers do not
// instantiate Config directly.
type Config struct {
	memtableMaxDocs  int
	memtableMaxBytes int
	walMaxBytes      int
	lockHeartbeat    time.Duration
	now              func() time.Time
	autoCompact      bool
	tokenizer        tokenize.Tokenizer

	compactInterval time.Duration
	compactMin      int
	compactMaxSize  int64
}

func defaultConfig() Config {
	return Config{
		memtableMaxDocs:  DefaultMemtableMaxDocs,
		memtableMaxBytes: DefaultMemtableMaxBytes,
		walMaxBytes:      DefaultWALMaxBytes,
		lockHeartbeat:    DefaultLockHeartbeat,
		now:              time.Now,
		autoCompact:      true,
		// CJKBigram falls back to Simple for ASCII so
		// it is a strict superset; choosing it as default avoids
		// "search returned 0 hits in Chinese" footguns at zero
		// cost on English-only corpora.
		tokenizer: &tokenize.CJKBigram{},

		compactInterval: DefaultCompactionInterval,
		compactMin:      DefaultCompactionMinSegments,
		compactMaxSize:  DefaultCompactionMaxSize,
	}
}

// Option configures an [Index] at construction time.
type Option func(*Config)

// WithMemtableMaxDocs overrides the doc-count flush threshold.
func WithMemtableMaxDocs(n int) Option {
	return func(c *Config) {
		if n > 0 {
			c.memtableMaxDocs = n
		}
	}
}

// WithMemtableMaxBytes overrides the byte-size flush threshold.
func WithMemtableMaxBytes(n int) Option {
	return func(c *Config) {
		if n > 0 {
			c.memtableMaxBytes = n
		}
	}
}

// WithWALMaxBytes overrides the WAL rotation threshold.
func WithWALMaxBytes(n int) Option {
	return func(c *Config) {
		if n > 0 {
			c.walMaxBytes = n
		}
	}
}

// WithLockHeartbeat overrides how often the writer refreshes its
// lockfile mtime. Decrease to detect crashed writers faster at the
// cost of more Workspace.Stat traffic.
func WithLockHeartbeat(d time.Duration) Option {
	return func(c *Config) {
		if d > 0 {
			c.lockHeartbeat = d
		}
	}
}

// WithClock substitutes the time source used for generation
// timestamps and lockfile heartbeat. Tests pass a deterministic
// clock; production code should leave the default.
func WithClock(now func() time.Time) Option {
	return func(c *Config) {
		if now != nil {
			c.now = now
		}
	}
}

// WithAutoCompact toggles the background size-tiered compactor.
// Default is true. With auto-compact disabled callers MUST drive
// merges themselves via [Index.Compact] or segment counts will
// grow unbounded.
func WithAutoCompact(on bool) Option { return func(c *Config) { c.autoCompact = on } }

// WithCompactionInterval overrides the background compactor's
// wake-up tick. Flushes also poke the compactor inline, so this
// only bounds worst-case lag for namespaces that have stopped
// receiving writes.
func WithCompactionInterval(d time.Duration) Option {
	return func(c *Config) {
		if d > 0 {
			c.compactInterval = d
		}
	}
}

// WithCompactionMinSegments sets the per-bucket segment count
// that triggers a merge. Lower values amplify writes; higher
// values amplify reads (because more segments must be opened per
// Search). Default 4 is the LSM literature's typical sweet spot.
func WithCompactionMinSegments(n int) Option {
	return func(c *Config) {
		if n >= 2 {
			c.compactMin = n
		}
	}
}

// WithCompactionMaxSize sets the per-segment byte budget at which
// compaction stops. Segments that already exceed this are not
// further merged; new segments produced by compaction inherit
// roughly the sum of the merged inputs.
func WithCompactionMaxSize(bytes int64) Option {
	return func(c *Config) {
		if bytes > 0 {
			c.compactMaxSize = bytes
		}
	}
}

// WithTokenizer overrides the BM25 tokenizer used for query parsing
// and segment-local corpus rebuild. Both sides MUST use the same
// tokenizer instance or query tokens won't match the indexed
// corpus; passing a single Tokenizer for both is the only supported
// configuration. Default is [tokenize.CJKBigram].
func WithTokenizer(t tokenize.Tokenizer) Option {
	return func(c *Config) {
		if t != nil {
			c.tokenizer = t
		}
	}
}

// Index is the [retrieval.Index] backed by a [sdkworkspace.Workspace].
//
// Construct via [New]. An Index may host any number of namespaces;
// each namespace is independent (its own manifest, segments, WAL,
// memtable, lockfile). Operations on different namespaces never
// contend.
type Index struct {
	ws  sdkworkspace.Workspace
	cfg Config

	// nsMu guards the namespaces map itself. Per-namespace state
	// has its own lock; nsMu is only held during create-on-first-
	// access of a new namespace.
	nsMu       sync.Mutex
	namespaces map[string]*namespaceState

	closed atomic.Bool

	// compactWake is buffered (cap 1) so flushLocked can post a
	// non-blocking "wake up, segments may need compaction" signal
	// without serialising on the worker loop.
	compactWake chan struct{}

	// compactDone closes when the worker goroutine exits. Close()
	// waits on it so callers see a clean shutdown — no orphaned
	// background work after the public Close returns.
	compactDone chan struct{}

	// compactCancel cancels the worker's root context so periodic
	// I/O (List, Read, Write, Rename, RemoveAll) returns promptly
	// on shutdown rather than hanging until the next manifest swap.
	compactCancel context.CancelFunc
}

// namespaceState carries the live state of one namespace. Held by
// pointer so callers can keep a reference across the surrounding
// nsMu being released.
type namespaceState struct {
	name  string
	paths pathHelper

	// rwMu serializes writes within this namespace and lets reads
	// run concurrently. Manifest swaps acquire the write lock.
	rwMu sync.RWMutex

	// compactMu serialises compaction tasks within this namespace.
	// Held for the entire compaction (read sources -> write dest
	// -> manifest swap -> retire). Held SEPARATELY from rwMu so
	// reading source segments and writing the destination segment
	// do not block writers; only the brief manifest swap takes
	// rwMu.
	compactMu sync.Mutex

	// manifest is the latest committed snapshot. Read under rwMu.
	manifest *manifest

	// memtable is the in-memory staging buffer that absorbs Upsert /
	// Delete batches between flushes. Swapped wholesale during
	// flush so reads observing the post-swap manifest see a
	// memtable that has already been drained.
	memtable *memtable

	// wal is the active write-ahead log writer; rotated during
	// flush so each flush corresponds to a contiguous range of
	// WAL sequence numbers.
	wal *walWriter

	// lockHolder identifies our acquire of the namespace's
	// .lock file. Empty means the locking protocol is disabled
	// (the workspace lacks AtomicRename); the heartbeat and
	// release paths skip such namespaces.
	lockHolder string

	// lockCancel cancels the heartbeat goroutine. nil when the
	// protocol is disabled. Called by Close.
	lockCancel context.CancelFunc

	// lockDone is closed when the heartbeat goroutine exits.
	// Close waits on this to drain the goroutine cleanly.
	lockDone chan struct{}

	// fenced is set to true when the heartbeat loop observes that
	// the lockfile no longer names us as holder — i.e., another
	// writer has taken over. Mutating / reading methods inspect
	// this via [fenceCheck] and refuse to proceed when set.
	fenced atomic.Bool

	// retired is set by Drop after the state is removed from the
	// namespace map but before the namespace directory is deleted.
	// Stale callers that already obtained this pointer must stop
	// instead of touching segment files that Drop is about to remove.
	retired atomic.Bool
}

// New constructs an [Index] on top of ws. The namespace state is
// lazy: the first Upsert / Search against a namespace performs
// directory creation, manifest read, and WAL replay. New itself only
// validates configuration.
//
// Pass nil for ws to get a clear validation error rather than a
// panic deeper in the call chain.
func New(ws sdkworkspace.Workspace, opts ...Option) (*Index, error) {
	if ws == nil {
		return nil, errNilWorkspace
	}
	cfg := defaultConfig()
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	idx := &Index{
		ws:          ws,
		cfg:         cfg,
		namespaces:  make(map[string]*namespaceState),
		compactWake: make(chan struct{}, 1),
		compactDone: make(chan struct{}),
	}
	if cfg.autoCompact {
		ctx, cancel := context.WithCancel(context.Background())
		idx.compactCancel = cancel
		go idx.compactionLoop(ctx)
	} else {
		// Even with autoCompact off we close compactDone so Close
		// can uniformly block on it without conditional plumbing.
		close(idx.compactDone)
	}
	return idx, nil
}

// Close releases per-namespace resources (WAL writers, lockfile
// heartbeats, background compactors) and marks the index as closed.
// Subsequent operations return [ErrClosed]. Close is idempotent.
//
// Close does NOT force a final flush — pending memtable contents
// remain in the WAL for the next opener to recover. Callers who
// want the memtable persisted should call [Index.Flush] before
// Close.
func (idx *Index) Close() error {
	if !idx.closed.CompareAndSwap(false, true) {
		return nil
	}
	if idx.compactCancel != nil {
		idx.compactCancel()
	}
	// Wait for the compaction worker to drain so an in-flight
	// merge cannot race a Close that is also closing WAL writers.
	<-idx.compactDone

	idx.nsMu.Lock()
	states := make([]*namespaceState, 0, len(idx.namespaces))
	for _, st := range idx.namespaces {
		states = append(states, st)
	}
	idx.namespaces = map[string]*namespaceState{}
	idx.nsMu.Unlock()
	for _, st := range states {
		// Stop the heartbeat first so we don't race a final tick
		// with the lock release Delete.
		if st.lockCancel != nil {
			st.lockCancel()
			<-st.lockDone
		}
		st.rwMu.Lock()
		if st.wal != nil {
			_ = st.wal.Close()
		}
		st.rwMu.Unlock()
		// Best-effort release: only deletes when the file still
		// names us as Holder, so a fenced Index never erases the
		// new holder's lockfile.
		idx.releaseLock(context.Background(), st)
	}
	return nil
}

// pokeCompactor schedules a non-blocking wake-up of the background
// worker. Called by [flushLocked] when a fresh segment has just
// landed and may push some bucket past the merge threshold.
//
// Safe to call when autoCompact is disabled — the channel is
// drained by no one, but the cap-1 buffer absorbs the post and
// the next poke replaces the queued one.
func (idx *Index) pokeCompactor() {
	select {
	case idx.compactWake <- struct{}{}:
	default:
	}
}

// Capabilities advertises what this backend supports. The retrieval
// algorithm side is fixed (BM25 + flat vectors + sparse dot product + hybrid fusion,
// full Filter operator coverage), but the storage-medium properties
// — atomicity, read-after-write, distribution — are derived from the
// underlying Workspace via [sdkworkspace.CapabilitiesOf]. Pointing
// the same Index at LocalWorkspace vs an object store therefore
// yields different (and accurate) Capabilities values.
func (idx *Index) Capabilities() retrieval.Capabilities {
	wsc := sdkworkspace.CapabilitiesOf(idx.ws)
	return retrieval.Capabilities{
		BM25:   true,
		Vector: true,
		Sparse: true,
		Hybrid: true,

		FilterPushdown: true,
		MaxFilterDepth: -1,
		SupportedOps: []string{
			"eq", "neq", "in", "nin", "range", "exists", "missing",
			"contains", "icontains", "contains_any", "contains_all",
			"and", "or", "not", "match",
		},

		BatchUpsertMax: 0,
		// WriteIsAtomic only when the underlying Workspace can do
		// atomic Rename — that is the primitive the manifest swap
		// relies on for atomic batch publication.
		WriteIsAtomic: wsc.AtomicRename,

		MaxListPageSize:      10_000,
		NativeDeleteByFilter: false,
		SupportedListOrders: []retrieval.ListOrderBy{
			retrieval.OrderByTimestampDesc,
			retrieval.OrderByTimestampAsc,
			retrieval.OrderByIDAsc,
		},

		// ReadAfterWrite and Distributed are pure passthroughs from
		// the workspace medium; nothing this Index does on top can
		// strengthen them.
		ReadAfterWrite: wsc.ReadAfterWrite,
		Distributed:    wsc.Distributed,
		Extensions: retrieval.ExtensionCapabilities{
			DocGetter:      true,
			Filterable:     true,
			Iterable:       true,
			Count:          true,
			DeleteByFilter: true,
			DropNamespace:  true,
		},
	}
}
