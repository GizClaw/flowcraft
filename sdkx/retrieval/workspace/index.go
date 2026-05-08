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
	name string

	// rwMu serializes writes within this namespace and lets reads
	// run concurrently. Manifest swaps acquire the write lock.
	rwMu sync.RWMutex

	// manifest is the latest committed snapshot. Read under rwMu.
	manifest *manifest

	// Implementation fields (memtable, walWriter, segment cache,
	// lockfile heartbeat goroutine) are added in subsequent commits
	// alongside the corresponding code paths.
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
func (idx *Index) Close() error {
	if !idx.closed.CompareAndSwap(false, true) {
		return nil
	}
	// Per-namespace teardown is filled in alongside the components
	// it owns: WAL writers (write-path commit), lockfile heartbeat
	// (concurrency commit), background compactor (compaction
	// commit). At this skeleton stage there is nothing to release.
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
