// Package journal implements the file journal contract in
// core/sandbox: a bounded, cursor-replayable record of the writes that
// happen inside a runner's write boundary.
//
// It is one engine shared by every backend, split into platform-neutral
// logic (folding, retention, gap accounting) and a [Source] — the only
// OS-specific part. There are three sources: inotify on Linux, where one
// watch covers a directory and the kernel names the entry that changed;
// kqueue on macOS, where a descriptor watches one entry and the source
// derives entry-level events by diffing the directories it re-reads; and
// ReadDirectoryChangesW on Windows, where one directory handle reports
// the entries that changed inside it through a read that is always
// pending on a completion port. Platforms without a source report
// sandbox.JournalCapabilities.Enabled = false and fail
// sandbox.OpenJournal with errdefs.NotAvailable rather than pretending
// to watch.
//
// # What it reports
//
// Net state changes, not syscall traces: a file created and written in
// one go is one create, a rename is one rename, a file that appears and
// disappears inside the de-duplication window is nothing at all, and a
// chmod — or a read — is never an event. fold.go holds the rules.
//
// # What it costs
//
// A journal that was never asked for costs nothing: no goroutine, no
// file descriptor, no watch. An attached journal costs one file
// descriptor, one goroutine, one kernel watch per directory under the
// watched roots (about 1 KB of kernel memory each) and one bounded
// in-memory ring. When the watch budget runs out the journal reports a
// capacity gap instead of silently watching less.
//
// On macOS the same promise is kept in the unit that platform charges
// for: kqueue has no recursive watch and no per-directory filter, so
// the source holds one descriptor per watched entry (directory or file)
// plus the snapshot a directory diff needs. The budget is derived from
// the descriptor ceiling, and the source raises the soft limit toward
// the budget while it can — a watch set that still does not fit is a
// capacity gap, never a quiet subset. A note then costs a re-read of the
// directory that reported it: one pass over the directory's own records,
// plus a stat for the entries those records cannot identify on their
// own, so the price of a change is the width of the directory it landed
// in.
//
// On Windows it is the buffer that costs. A watch is one directory
// handle plus one read left pending on it, and the change buffer that
// read owns stays with the kernel until the read completes: 16 KiB of
// user memory and the same again pinned, per watched directory. The
// budget bounds that memory rather than a kernel limit — 4096 watches is
// 64 MiB of buffers and 64 MiB more pinned — which makes it the number a
// deployment is most likely to want to lower, and a watch set that does
// not fit is a capacity gap like anywhere else. A directory that
// produces more changes than its buffer can describe is the platform's
// own overflow report, and it becomes a gap too.
//
// # What each platform cannot see
//
// kqueue reports that an entry changed, not how: there is no close
// edge, and a file's content change is only visible through a
// descriptor on the file itself. Appearances and content changes
// therefore settle in the fold's window instead of being reported at
// the writer's close, a truncate arrives as an attribute change (with
// the size as the evidence), and a move is paired by inode identity
// rather than by a kernel cookie: a rename the journal can see the
// inode survive is one rename, and a departure whose entry said nothing
// is reported as a deletion. Two things are better than inotify's:
// notes coalesce but are never dropped, so there is no queue overflow
// to report, and a coalesced change is a merged report rather than a
// lost one.
//
// Windows reports entries, not the directories they are in, and has no
// close edge either, so appearances and content changes settle in the
// fold's window. It names an entry without saying what that entry is, so
// every arrival costs one stat to learn whether it is a directory — the
// one answer the source may not get wrong, because a directory the
// engine does not recognize as one is a subtree it never watches. A
// rename is one event when the platform writes both halves adjacently in
// one read, which is what a rename inside one directory produces; a move
// between two directories, or out of the tree, arrives as two records
// with no cookie joining them, and is reported as the removal and the
// appearance it also is rather than as a rename nobody proved. A
// directory whose own timestamp changed — its entry set moved — arrives
// as a modification of that directory and is dropped: that directory's
// own watch reports the change itself, in detail. Content changes are
// the platform's own: it records a size or last-write change when the
// filesystem sees the write, so a writer that keeps its bytes in the
// cache has its write reported late, never wrongly. Dev and Ino stay
// zero: a file's identity there is a volume serial number plus a file
// index reached through the file's own handle, which is not a stat.
//
// # What it is not
//
// Not an enforcement boundary (the backend's write confinement is), not
// an audit log (events can be missing, and the window is bounded), and
// not an attribution mechanism: nothing in an inotify stream names the
// writer, so WriteEvent.Session stays empty instead of guessing from
// timing.
//
// # Files
//
//   - Contract: core/sandbox/journal.go (FileJournal / WriteEvent / gaps)
//   - Engine: journal.go (lifecycle), ring.go (window + loss ledger),
//     fold.go (net-change rules)
//   - Sources: source.go (the Source interface), source_linux.go
//     (inotify), source_darwin.go (kqueue + directory diffs),
//     source_windows.go (ReadDirectoryChangesW + a completion port),
//     source_other.go (the no-source stub)
//   - Deployment: settings.go (the `journal:` subtree)
package journal
