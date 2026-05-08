package workspace

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// Defaults applied when the corresponding [Option] is not supplied.
// Tuned for a few-thousand-doc namespace running on LocalWorkspace;
// callers with very different shapes (mostly-read, very-write-heavy,
// large vectors) should pass explicit options.
const (
	// DefaultRoot is the empty string, which means "use workspace
	// root". Set [WithRoot] to nest the index under a sub-path.
	DefaultRoot = ""

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
)

// Config bundles the resolved configuration of an [Index]. It is
// produced from a slice of [Option] values; callers do not
// instantiate Config directly.
type Config struct {
	root             string
	memtableMaxDocs  int
	memtableMaxBytes int
	walMaxBytes      int
	lockHeartbeat    time.Duration
	now              func() time.Time
	autoCompact      bool
}

func defaultConfig() Config {
	return Config{
		root:             DefaultRoot,
		memtableMaxDocs:  DefaultMemtableMaxDocs,
		memtableMaxBytes: DefaultMemtableMaxBytes,
		walMaxBytes:      DefaultWALMaxBytes,
		lockHeartbeat:    DefaultLockHeartbeat,
		now:              time.Now,
		autoCompact:      true,
	}
}

// Option configures an [Index] at construction time.
type Option func(*Config)

// WithRoot nests the index under a sub-path of the workspace, which
// lets a single Workspace host the index next to recall/, knowledge/,
// memories/, history/ subtrees without name collisions. The default
// is "" (workspace root).
func WithRoot(p string) Option { return func(c *Config) { c.root = p } }

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
// Default is true. Disable for tests that want to assert raw
// segment layout without compaction reshuffling it.
func WithAutoCompact(on bool) Option { return func(c *Config) { c.autoCompact = on } }

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
	return &Index{
		ws:         ws,
		cfg:        cfg,
		namespaces: make(map[string]*namespaceState),
	}, nil
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
	idx.nsMu.Lock()
	states := make([]*namespaceState, 0, len(idx.namespaces))
	for _, st := range idx.namespaces {
		states = append(states, st)
	}
	idx.namespaces = map[string]*namespaceState{}
	idx.nsMu.Unlock()
	for _, st := range states {
		st.rwMu.Lock()
		if st.wal != nil {
			_ = st.wal.Close()
		}
		st.rwMu.Unlock()
	}
	return nil
}

// Capabilities advertises what this backend supports. The retrieval
// algorithm side is fixed (BM25 + flat vectors + hybrid via RRF,
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
		NativeDeleteByFilter: true,
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
		Debug:          false,
	}
}
