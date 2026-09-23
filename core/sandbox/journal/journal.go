package journal

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

const (
	// defaultSweepInterval is how often the engine wakes up when no
	// event arrives, which bounds how late a folded change whose
	// completion edge never came is reported.
	defaultSweepInterval = 100 * time.Millisecond
	// maxRetention bounds the replay window. Retention is a memory
	// commitment (each event is a struct plus its paths), and a typo in
	// a settings file must fail the build rather than the host.
	maxRetention = 1 << 20
	// separator is the host path separator, used for prefix checks on
	// absolute paths.
	separator = string(os.PathSeparator)
)

// Config is the resolved construction input of a journal.
type Config struct {
	// Root is the runner's root directory: it is watched recursively,
	// and event paths under it are reported relative to it.
	Root string
	// ExtraRoots are additional subtrees to watch — the runner's
	// explicitly writable paths outside the root. Events there carry
	// absolute paths. Entries inside Root are ignored: the root walk
	// already covers them.
	ExtraRoots []string
	// Options is the contract-level configuration.
	Options sandbox.JournalOptions

	// The remaining fields are internal seams for tests: the idle
	// wakeup interval, the clock, and the fold's deadline. They are
	// read once, before the watch goroutine starts.
	sweep time.Duration
	clock func() time.Time
	ttl   time.Duration
}

// Capabilities reports the journal surface this build can offer. Each
// field is a platform fact, so the ones that differ per platform live
// with the code that answers them rather than here.
//
// Enabled is always false: it is an instance fact that the owning runner
// fills in once it has a journal attached.
func Capabilities() sandbox.JournalCapabilities {
	if !Available() {
		return sandbox.JournalCapabilities{}
	}
	return sandbox.JournalCapabilities{
		// Every source pairs the moves it can prove: inotify through the
		// cookie the kernel attaches, kqueue through the inode that
		// survives, ReadDirectoryChangesW through the pair it writes
		// side by side. What each of them cannot prove — a destination
		// outside the watched tree, or on Windows a move between two
		// directories — is reported as the removal and the appearance it
		// also is rather than as a rename.
		RenamePairing: true,
		FileIdentity:  fileIdentityAvailable(),
		WatchBudget:   defaultBudget(),
	}
}

// Available reports whether this build has a watch source. Callers use
// it to fail a deployment early instead of building a runner that
// quietly reports nothing.
func Available() bool { return available() }

// Capabilities reports the journal surface of this instance: the
// platform's facts with Enabled set, and the zero value when no journal
// is attached. It is a nil-safe method so a backend can report through
// it unconditionally:
//
//	Policy: ..., Features: ..., Journal: r.journal.Capabilities()
//
// A nil receiver answers "no journal" without special-casing at the
// call site.
func (j *Journal) Capabilities() sandbox.JournalCapabilities {
	if j == nil {
		return sandbox.JournalCapabilities{}
	}
	caps := Capabilities()
	caps.Enabled = true
	// WatchBudget is an instance fact too: a deployment that set
	// max_watch_set registers that many watches before it reports
	// capacity gaps, so the declaration has to carry the budget in
	// force rather than the platform default.
	if j.budget > 0 {
		caps.WatchBudget = j.budget
	}
	return caps
}

// Journal is a running file journal: a bound watch set, a folded event
// stream and a bounded replay window. It is created by a backend at
// runner construction and closed with the runner.
type Journal struct {
	mu      sync.Mutex
	root    string
	exclude []string
	ops     []sandbox.FileOp
	budget  int
	ring    *ring
	fold    *fold
	src     Source
	// leaf is the source's optional per-entry watch (macOS kqueue);
	// nil when the platform's directory watch already reports
	// everything the source can see.
	leaf leafSource
	// dirs and byPath are the subscription bookkeeping: handle to
	// absolute watched path, and back. A path is a directory on every
	// platform, and a non-directory entry on a source that watches
	// leaves. The engine owns the paths because it must keep them
	// correct when a watched subtree moves (the platform watches
	// follow the inode).
	dirs   map[Handle]string
	byPath map[string]Handle
	// roots are the top-level watch roots. A watch root has no parent
	// watch to report its removal, so its own delete-self event does.
	roots   []string
	frozen  bool
	readers int

	closing atomic.Bool
	done    chan struct{}

	// now and sweep are seams for tests: the engine's clock and its
	// idle wakeup interval.
	now   func() time.Time
	sweep time.Duration
}

// New starts a journal. It fails with errdefs.NotAvailable when this
// platform has no watch source, and with errdefs.Validation for
// unusable configuration.
func New(cfg Config) (*Journal, error) {
	src, err := openSource()
	if err != nil {
		return nil, err
	}
	j, err := newWithSource(cfg, src)
	if err != nil {
		_ = src.Close()
		return nil, err
	}
	return j, nil
}

// newWithSource is the shared constructor: New hands it the platform
// source, tests hand it a fake.
func newWithSource(cfg Config, src Source) (*Journal, error) {
	if src == nil {
		return nil, errdefs.Validationf("sandbox/journal: source is required")
	}
	resolved, err := resolveConfig(cfg)
	if err != nil {
		return nil, err
	}

	j := &Journal{
		root:    resolved.root,
		exclude: resolved.exclude,
		ops:     resolved.ops,
		budget:  resolved.budget,
		ring:    newRing(resolved.retention),
		fold:    newFold(resolved.ops, pendingTTL),
		src:     src,
		dirs:    make(map[Handle]string),
		byPath:  make(map[string]Handle),
		done:    make(chan struct{}),
		now:     time.Now,
		sweep:   defaultSweepInterval,
	}
	if cfg.sweep > 0 {
		j.sweep = cfg.sweep
	}
	if cfg.clock != nil {
		j.now = cfg.clock
	}
	if cfg.ttl > 0 {
		j.fold.ttl = cfg.ttl
	}
	if leaf, ok := src.(leafSource); ok {
		j.leaf = leaf
	}

	// Watch roots first, then the extra subtrees. Nothing is seeded
	// here: files that already existed when the runner was built are
	// not writes this journal observed.
	j.roots = append(j.roots, resolved.root)
	j.watchTree(resolved.root, false, j.now())
	for _, extra := range cfg.ExtraRoots {
		dir, err := resolveDir(resolved.root, extra)
		if err != nil {
			// The runner was built with a writable path that is no
			// longer usable: watch what we can and say so, rather
			// than fail the whole runner.
			j.ring.recordLoss(sandbox.JournalGapCapacity)
			continue
		}
		if dir == resolved.root || strings.HasPrefix(dir, resolved.root+separator) {
			continue
		}
		j.roots = append(j.roots, dir)
		j.watchTree(dir, false, j.now())
	}

	go j.run()
	return j, nil
}

// Open returns a new reader on the journal. Every reader holds its own
// cursor, so two consumers never steal events from each other.
func (j *Journal) Open() (*Reader, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.frozen {
		return nil, errdefs.NotAvailablef("sandbox/journal: journal is closed")
	}
	j.readers++
	return &Reader{j: j}, nil
}

// Close stops watching and freezes the journal. Retained events stay
// readable through the readers that were opened before, which see
// JournalBatch.Closed set and can drain the residue. Close is
// idempotent and safe to call concurrently.
func (j *Journal) Close() error {
	first := !j.closing.Swap(true)
	var err error
	if first {
		// Closing the source unblocks Poll, which lets the watch
		// goroutine exit and its final events reach the ring before we
		// freeze it.
		err = j.src.Close()
	}
	<-j.done

	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.frozen {
		j.freezeLocked()
	}
	return err
}

func (j *Journal) readBatch(ctx context.Context, afterSeq int64, max int) (sandbox.JournalBatch, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return sandbox.JournalBatch{}, err
		}
	}
	if afterSeq < 0 {
		return sandbox.JournalBatch{}, errdefs.Validationf(
			"sandbox/journal: afterSeq %d is negative", afterSeq)
	}
	if max <= 0 {
		return sandbox.JournalBatch{}, errdefs.Validationf(
			"sandbox/journal: max %d must be positive", max)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.ring.readView(afterSeq, max, j.frozen), nil
}

// Reader is one cursor into a journal. Closing it releases the handle;
// the journal itself stops when the owning runner closes.
type Reader struct {
	j      *Journal
	closed atomic.Bool
}

var _ sandbox.FileJournal = (*Reader)(nil)

// Read implements sandbox.FileJournal.
func (r *Reader) Read(ctx context.Context, afterSeq int64, max int) (sandbox.JournalBatch, error) {
	if r.closed.Load() {
		return sandbox.JournalBatch{}, errdefs.Validationf(
			"sandbox/journal: read on a closed reader")
	}
	return r.j.readBatch(ctx, afterSeq, max)
}

// Close implements sandbox.FileJournal. Once every reader of a frozen
// journal is closed the retained window is dropped: the runner is gone,
// and there is nobody left to replay for.
func (r *Reader) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	r.j.mu.Lock()
	defer r.j.mu.Unlock()
	r.j.readers--
	if r.j.frozen && r.j.readers == 0 {
		r.j.ring.release()
	}
	return nil
}

// run is the single watch goroutine: poll, fold, sweep, repeat.
func (j *Journal) run() {
	defer close(j.done)
	for {
		events, err := j.src.Poll(j.sweep)
		if err == nil || len(events) > 0 {
			// Folded before the shutdown check on purpose: the events
			// of this batch were already taken off the kernel queue, and
			// dropping them because Close raced with the poll would be
			// the one kind of loss this journal never reports.
			j.ingest(events)
		}
		if j.closing.Load() {
			return
		}
		if err != nil {
			// The source stopped on its own. Coverage ends here, and
			// pretending otherwise would be the silent hole this
			// package exists to avoid.
			j.mu.Lock()
			j.ring.recordLoss(sandbox.JournalGapBackend)
			j.freezeLocked()
			j.mu.Unlock()
			return
		}
	}
}

// ingest folds one batch of raw events and sweeps the deadlines that
// came due with it.
func (j *Journal) ingest(events []rawEvent) {
	now := j.now()
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, ev := range events {
		j.observeLocked(ev, now)
	}
	var out []change
	for _, gap := range j.fold.Sweep(&out, now) {
		j.ring.recordLoss(gap)
	}
	j.applyLocked(out, now)
}

// freezeLocked ends coverage: whatever the fold was still holding gets
// reported (nothing will ever complete it), and readers learn from
// Closed that the stream is over.
func (j *Journal) freezeLocked() {
	var out []change
	j.fold.Flush(&out)
	for _, c := range out {
		j.emitLocked(c, j.now())
	}
	j.frozen = true
}

// observeLocked handles one raw event: the journal-level events
// (overflow, watch teardown) here, everything else through the fold.
func (j *Journal) observeLocked(ev rawEvent, now time.Time) {
	switch ev.Op {
	case rawOverflow:
		// The kernel dropped events before we saw them, and it does not
		// say how many. The boundary is the current high watermark: the
		// numbers we do have are real, coverage after them is not.
		j.ring.recordLoss(sandbox.JournalGapOverflow)
		return
	case rawDeleteSelf:
		j.deletedSelfLocked(ev.Handle, now)
		return
	case rawIgnored:
		j.watchLostLocked(ev.Handle, now)
		return
	}
	dir, ok := j.dirs[ev.Handle]
	if !ok {
		// A watch we already removed: events queued before Remove are
		// not losses, the change they describe is gone with the tree
		// that held it.
		return
	}
	path := dir
	if ev.Name != "" {
		path = filepath.Join(dir, ev.Name)
	}
	switch ev.Op {
	case rawCreate, rawMovedTo:
		// The entry is known the moment it is observed, so its content
		// watch starts here rather than when the fold gets around to
		// reporting it: a file that is written and renamed (or unlinked)
		// inside the fold's window still gets its own evidence, and its
		// writer's later writes are watched from the start.
		if !ev.IsDir {
			j.watchLeaf(path)
		}
	}
	j.applyLocked(j.fold.Observe(ev, path, now), now)
}

// deletedSelfLocked handles the directory's own removal. Inside the
// tree this is expected: the parent's watch already reported the
// removal of the entry, so repeating it would be a duplicate. A watch
// root has no parent watch, so there it is the only report.
func (j *Journal) deletedSelfLocked(h Handle, now time.Time) {
	dir, ok := j.dirs[h]
	if !ok {
		return
	}
	top := j.isRoot(dir)
	j.unwatchLocked(dir)
	if top {
		j.emitLocked(change{op: sandbox.FileOpRemove, path: dir, isDir: true}, now)
	}
}

// watchLostLocked handles a subscription the kernel dropped without
// being asked: whatever happens under that path from now on is
// invisible, which is a coverage gap, not a quiet ending.
func (j *Journal) watchLostLocked(h Handle, now time.Time) {
	dir, ok := j.dirs[h]
	if !ok {
		return
	}
	j.unwatchLocked(dir)
	j.emitLocked(change{op: sandbox.FileOpRemove, path: dir, isDir: true}, now)
	j.ring.recordLoss(sandbox.JournalGapWatchLost)
}

// applyLocked turns folded changes into events and keeps the watch set
// in step with the filesystem.
func (j *Journal) applyLocked(changes []change, now time.Time) {
	for _, c := range changes {
		switch {
		case c.op == sandbox.FileOpRename && c.isDir:
			// The rename is reported before the subtree is re-registered:
			// a consumer replaying the stream should learn that a
			// directory moved before it learns about the entries that
			// moved with it.
			j.emitLocked(c, now)
			j.reparentLocked(c.oldPath, c.path, now)
		case c.op == sandbox.FileOpRemove:
			// A removed entry's subscription ends with it: the source
			// may have retired it already (a leaf whose own name left),
			// and dropping the handle keeps the budget honest. For a
			// directory this first tears the whole subtree down.
			j.unwatchLocked(c.path)
			j.emitLocked(c, now)
		case c.op == sandbox.FileOpRename && !c.isDir:
			// A renamed file keeps its inode but not its path: the old
			// subscription goes. The new name's leaf is registered
			// where the arrival was observed, not here.
			j.emitLocked(c, now)
			j.unwatchLocked(c.oldPath)
		default:
			// A directory create is reported before its subtree is
			// registered and seeded, for the same reason: parents first,
			// children after, which is the order the filesystem changed
			// in.
			j.emitLocked(c, now)
			if c.op == sandbox.FileOpCreate && c.isDir {
				j.watchTree(c.path, true, now)
			}
		}
	}
}

// emitLocked appends one event to the ring, filling in the identity
// hints the platform can give.
func (j *Journal) emitLocked(c change, now time.Time) {
	if !j.fold.wants(c.op) {
		return
	}
	ev := sandbox.WriteEvent{
		At:      now,
		Op:      c.op,
		Path:    j.eventPath(c.path),
		IsDir:   c.isDir,
		Size:    -1,
		Session: "", // Linux inotify cannot name the writer; see the package doc.
	}
	if c.oldPath != "" {
		ev.OldPath = j.eventPath(c.oldPath)
	}
	if c.op != sandbox.FileOpRemove {
		if info, err := os.Lstat(c.path); err == nil {
			ev.IsDir = ev.IsDir || info.IsDir()
			if dev, ino, ok := fileIdentity(info); ok {
				ev.Dev, ev.Ino = dev, ino
			}
			if !info.IsDir() {
				ev.Size = info.Size()
			}
		}
	}
	j.ring.append(ev)
}

// eventPath rewrites an absolute host path into the journal's path
// shape: relative to the runner root with "/" separators, and a cleaned
// absolute path for the explicitly writable subtrees that live outside
// the root.
func (j *Journal) eventPath(abs string) string {
	if rel, err := filepath.Rel(j.root, abs); err == nil {
		if rel == "." {
			return "."
		}
		if !strings.HasPrefix(rel, ".."+separator) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(filepath.Clean(abs))
}

// watchTree registers every directory under dir, respecting the exclude
// list and the watch budget. seed makes it report the entries it finds:
// the caller is watching a subtree that just appeared, so its contents
// are appearances too.
func (j *Journal) watchTree(dir string, seed bool, now time.Time) {
	if j.isExcluded(dir) {
		return
	}
	queue := []string{dir}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if j.isExcluded(next) {
			continue
		}
		if _, ok := j.byPath[next]; ok {
			continue
		}
		if j.overBudget() {
			// The watch set is full: everything below this point is
			// invisible, and that is exactly what a capacity gap says.
			j.ring.recordLoss(sandbox.JournalGapCapacity)
			return
		}
		h, err := j.src.Add(next)
		if err != nil {
			j.ring.recordLoss(sandbox.JournalGapCapacity)
			continue
		}
		j.dirs[h] = next
		j.byPath[next] = h

		entries, err := os.ReadDir(next)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			child := filepath.Join(next, entry.Name())
			isDir := entry.IsDir()
			if isDir {
				queue = append(queue, child)
			} else {
				j.watchLeaf(child)
			}
			if !seed {
				continue
			}
			var out []change
			j.fold.Seed(&out, child, isDir, now)
			j.applyLocked(out, now)
		}
	}
}

// watchLeaf registers one non-directory entry, on a source that watches
// leaves. Nothing happens on a source that does not: the platform's
// directory watch reports everything that source can see, and asking
// for more would be a watch nobody uses.
func (j *Journal) watchLeaf(path string) {
	if j.leaf == nil {
		return
	}
	if j.overBudget() {
		// Reported once per change, exactly like a directory that did
		// not fit: the gap says the watch set is incomplete from here.
		j.ring.recordLoss(sandbox.JournalGapCapacity)
		return
	}
	if h, ok := j.byPath[path]; ok {
		// The name is watched already but now holds a different entry
		// (a rename destination, an inode swapped under it): the stale
		// subscription goes before the new one takes the path.
		delete(j.byPath, path)
		delete(j.dirs, h)
		_ = j.src.Remove(h)
	}
	h, err := j.leaf.AddLeaf(path)
	if err != nil {
		// An entry that is gone, that is a directory after all, or
		// that has no content to change is not a capacity problem: the
		// directory diff that produced the name reports what happened
		// to it.
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errNoLeaf) {
			j.ring.recordLoss(sandbox.JournalGapCapacity)
		}
		return
	}
	j.dirs[h] = path
	j.byPath[path] = h
}

// overBudget reports that the watch set is full: directories and leaves
// share the same budget, because they share the same subscriptions.
func (j *Journal) overBudget() bool {
	return j.budget > 0 && len(j.dirs) >= j.budget
}

// unwatchLocked stops watching a subtree and drops its folded state.
func (j *Journal) unwatchLocked(dir string) {
	j.fold.Forget(dir)
	for h, path := range j.dirs {
		if path == dir || strings.HasPrefix(path, dir+separator) {
			delete(j.dirs, h)
			delete(j.byPath, path)
			_ = j.src.Remove(h)
		}
	}
}

// reparentLocked moves the subscriptions of a renamed directory. The
// platform's watches follow the inode, so only the engine's idea of the
// paths needs to change — for the whole subtree, which is why the map
// is keyed by handle.
func (j *Journal) reparentLocked(old, new string, now time.Time) {
	if old == "" || old == new {
		return
	}
	j.fold.Reparent(old, new)
	for h, path := range j.dirs {
		switch {
		case path == old:
			delete(j.byPath, path)
			j.dirs[h] = new
			j.byPath[new] = h
		case strings.HasPrefix(path, old+separator):
			next := new + path[len(old):]
			delete(j.byPath, path)
			j.dirs[h] = next
			j.byPath[next] = h
		}
	}
	// A directory that was excluded on the way in is unwatched, and a
	// rename can carry it somewhere the exclude list does not cover:
	// register it now, or the subtree stays invisible.
	if _, ok := j.byPath[new]; !ok {
		j.watchTree(new, true, now)
	}
}

func (j *Journal) isExcluded(path string) bool {
	for _, ex := range j.exclude {
		if path == ex || strings.HasPrefix(path, ex+separator) {
			return true
		}
	}
	return false
}

func (j *Journal) isRoot(path string) bool {
	for _, root := range j.roots {
		if path == root {
			return true
		}
	}
	return false
}

// resolved is the validated, path-resolved form of a [Config].
type resolved struct {
	root      string
	exclude   []string
	ops       []sandbox.FileOp
	retention int
	budget    int
}

// resolveConfig validates a construction request and resolves every
// path in it. Both the engine and the deployment-settings entry point
// run through here, so a journal that starts is a journal that passed
// the same checks the build did.
func resolveConfig(cfg Config) (resolved, error) {
	if cfg.Root == "" {
		return resolved{}, errdefs.Validationf("sandbox/journal: root is required")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return resolved{}, errdefs.Validationf("sandbox/journal: resolve root: %v", err)
	}
	if resolved, evalErr := filepath.EvalSymlinks(root); evalErr == nil {
		root = resolved
	}
	if !isDir(root) {
		return resolved{}, errdefs.Validationf("sandbox/journal: root %q is not a directory", cfg.Root)
	}

	retention := cfg.Options.Retention
	if retention <= 0 {
		retention = sandbox.DefaultJournalRetention
	}
	if retention > maxRetention {
		return resolved{}, errdefs.Validationf(
			"sandbox/journal: retention %d exceeds the %d event limit", retention, maxRetention)
	}
	ops, err := normalizeOps(cfg.Options.Ops)
	if err != nil {
		return resolved{}, err
	}
	exclude, err := resolveExclude(root, cfg.Options.Exclude)
	if err != nil {
		return resolved{}, err
	}
	budget := cfg.Options.MaxWatchSet
	if budget <= 0 {
		budget = defaultBudget()
	}
	return resolved{root: root, exclude: exclude, ops: ops, retention: retention, budget: budget}, nil
}

// normalizeOps validates the op filter. An unknown value is a
// configuration error, not something to ignore: a typo in an op name
// would silently change what the journal reports.
func normalizeOps(ops []sandbox.FileOp) ([]sandbox.FileOp, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	seen := make(map[sandbox.FileOp]bool, len(ops))
	out := make([]sandbox.FileOp, 0, len(ops))
	for _, op := range ops {
		if op > sandbox.FileOpRemove {
			return nil, errdefs.Validationf("sandbox/journal: unknown file op %d", op)
		}
		if seen[op] {
			continue
		}
		seen[op] = true
		out = append(out, op)
	}
	return out, nil
}

// resolveExclude turns root-relative exclude entries into absolute
// paths. An entry that escapes the root is rejected: excluding
// something outside the watched tree would be silently meaningless.
func resolveExclude(root string, entries []string) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		if filepath.IsAbs(entry) {
			return nil, errdefs.Validationf(
				"sandbox/journal: exclude %q must be relative to the runner root", entry)
		}
		abs := filepath.Clean(filepath.Join(root, entry))
		if abs != root && !strings.HasPrefix(abs, root+separator) {
			return nil, errdefs.Validationf(
				"sandbox/journal: exclude %q resolves outside the runner root", entry)
		}
		if abs == root {
			return nil, errdefs.Validationf(
				"sandbox/journal: exclude %q would exclude the runner root itself", entry)
		}
		out = append(out, abs)
	}
	return out, nil
}

// resolveDir resolves one extra watch root.
func resolveDir(root, dir string) (string, error) {
	abs := dir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, dir)
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if !isDir(abs) {
		return "", errdefs.Validationf("sandbox/journal: %q is not a directory", dir)
	}
	return abs, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
