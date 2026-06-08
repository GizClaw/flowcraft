package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// Locking is best-effort and protocol-only: it does not depend on
// flock(2) or any other syscall, just [sdkworkspace.Workspace]
// primitives (Read, Write, Rename, Delete). The protocol assumes
// AtomicRename; when the workspace lacks it, the protocol is
// disabled (see [acquireLock]) and single-writer use becomes the
// caller's responsibility.

// staleMultiplier is how many heartbeat intervals must elapse
// before another writer treats a lockfile as abandoned. 3× lets
// one missed tick (e.g., a GC pause) survive without a takeover
// while still bounding crash-recovery latency to a small multiple
// of WithLockHeartbeat.
const staleMultiplier = 3

// acquireLock implements the protocol's "open or take over" step.
// On success it writes a fresh lockState whose Holder is unique
// to this Index instance, ready for the heartbeat loop to refresh.
//
// Returns:
//   - lockState (current holder is now us), nil on success;
//   - zero-value, [ErrLocked] if a live writer holds the file;
//   - zero-value, wrapped error for I/O / corruption issues.
//
// When the workspace does not advertise [sdkworkspace.Capabilities.AtomicRename]
// the protocol is no-op: we return a zero-value lockState with
// Holder=="" and a nil error. Callers detect this via the empty
// Holder and skip the heartbeat loop. The package doc warns that
// such workspaces require single-writer use.
func acquireLock(
	ctx context.Context,
	ws sdkworkspace.Workspace,
	paths pathHelper,
	heartbeat time.Duration,
	now time.Time,
) (lockState, error) {
	if !sdkworkspace.CapabilitiesOf(ws).AtomicRename {
		// Protocol disabled. Return a sentinel zero-value so the
		// caller can branch on Holder=="".
		return lockState{}, nil
	}

	for attempt := 0; attempt < 2; attempt++ {
		current, exists, err := readLockState(ctx, ws, paths)
		if err != nil {
			return lockState{}, err
		}
		if exists {
			// Live lock?  Someone refreshed within the staleness
			// window: refuse the acquire.
			if !isStale(current, heartbeat, now) {
				return lockState{}, ErrLocked
			}
			// Stale: fall through to takeover-write.
		}
		// Either no file, or the file is stale / corrupt.
		newState := newLockState(now)
		if err := writeLockState(ctx, ws, paths, newState); err != nil {
			return lockState{}, fmt.Errorf("acquireLock: write: %w", err)
		}
		// Read back to confirm we won the race against another
		// concurrent writer. Without a CAS primitive this is the
		// best we can do; in practice two takeovers happening in
		// the same millisecond against the same stale lock is
		// rare enough that "last writer wins" suffices.
		readBack, ok, err := readLockState(ctx, ws, paths)
		if err != nil {
			return lockState{}, err
		}
		if !ok || readBack.Holder != newState.Holder {
			// Lost the race. Retry once: the other writer is
			// presumably alive now, and the next iteration's
			// staleness check will reject our acquire.
			continue
		}
		return newState, nil
	}
	return lockState{}, ErrLocked
}

// readLockState fetches the current .lock contents. exists=false
// (with err=nil) means the file is absent — a fresh namespace.
// Corrupt files return exists=true with the zero-value state so
// the caller's staleness branch (always true for zero
// HeartbeatAt) takes over and overwrites them.
func readLockState(
	ctx context.Context,
	ws sdkworkspace.Workspace,
	paths pathHelper,
) (lockState, bool, error) {
	raw, err := ws.Read(ctx, paths.lockPath())
	if err != nil {
		if errors.Is(err, sdkworkspace.ErrNotFound) {
			return lockState{}, false, nil
		}
		return lockState{}, false, fmt.Errorf("readLockState: %w", err)
	}
	var st lockState
	if err := json.Unmarshal(raw, &st); err != nil {
		// Corrupt: report exists=true with a stale zero-value so
		// acquireLock falls into the takeover branch.
		return lockState{}, true, nil
	}
	if st.Version != lockStateVersion {
		// Schema mismatch: ditto.
		return lockState{}, true, nil
	}
	return st, true, nil
}

// writeLockState writes st via .lock.tmp + Rename so concurrent
// readers never observe a partially-serialised lockfile.
func writeLockState(
	ctx context.Context,
	ws sdkworkspace.Workspace,
	paths pathHelper,
	st lockState,
) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("writeLockState: marshal: %w", err)
	}
	if err := ws.Write(ctx, paths.lockTmpPath(), data); err != nil {
		return fmt.Errorf("writeLockState: write tmp: %w", err)
	}
	if err := ws.Rename(ctx, paths.lockTmpPath(), paths.lockPath()); err != nil {
		return fmt.Errorf("writeLockState: rename: %w", err)
	}
	return nil
}

// isStale reports whether st's heartbeat is old enough to qualify
// for takeover. Zero HeartbeatAt counts as stale (which is what
// makes corrupt-file recovery straightforward).
func isStale(st lockState, heartbeat time.Duration, now time.Time) bool {
	if st.HeartbeatAt.IsZero() {
		return true
	}
	return now.Sub(st.HeartbeatAt) > heartbeat*staleMultiplier
}

// newLockState mints a fresh state with a unique Holder.
func newLockState(now time.Time) lockState {
	return lockState{
		Version:     lockStateVersion,
		Holder:      newHolderID(),
		PID:         os.Getpid(),
		Hostname:    osHostname(),
		AcquiredAt:  now,
		HeartbeatAt: now,
	}
}

// newHolderID returns a 32-char hex token. Sufficiently long that
// two independent Index instances are astronomically unlikely to
// collide.
func newHolderID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Should never fail; fall back to a timestamp-based
		// identifier so we always return SOMETHING unique-ish.
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// osHostname is a guarded wrapper around os.Hostname so the lock
// build never fails in a sandboxed environment that denies the
// syscall.
func osHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// runHeartbeat refreshes the lockfile every heartbeat interval.
// The loop owns ctx; cancelling it stops the goroutine. On exit
// (cancel OR detected fence), done is closed.
//
// Detected fence flow:
//
//   - The loop reads the current lockfile each tick. If Holder
//     no longer matches our holderID, another writer has taken
//     over: we set st.fenced = true and exit. Subsequent
//     mutations on the namespace observe the fenced flag and
//     return [ErrFenced].
func (idx *Index) runHeartbeat(
	ctx context.Context,
	st *namespaceState,
	holder string,
	done chan<- struct{},
) {
	defer close(done)

	tick := time.NewTicker(idx.cfg.lockHeartbeat)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if st.fenced.Load() {
			return
		}
		// Read current lock; if Holder is no longer ours, we have
		// been fenced. We do NOT attempt to rewrite over a peer
		// holder — that would be a double-takeover race.
		current, ok, err := readLockState(ctx, idx.ws, st.paths)
		if err != nil {
			// Transient I/O: skip this tick, try again next.
			continue
		}
		if !ok || current.Holder != holder {
			st.fenced.Store(true)
			return
		}
		current.HeartbeatAt = idx.cfg.now()
		_ = writeLockState(ctx, idx.ws, st.paths, current)
	}
}

// releaseLock deletes the lockfile, but only when the current
// content still names us as Holder. This stops a fenced Index from
// erasing the new holder's lockfile on Close.
func (idx *Index) releaseLock(ctx context.Context, st *namespaceState) {
	if st.lockHolder == "" {
		return
	}
	current, ok, err := readLockState(ctx, idx.ws, st.paths)
	if err != nil || !ok {
		return
	}
	if current.Holder != st.lockHolder {
		return
	}
	_ = idx.ws.Delete(ctx, st.paths.lockPath())
}

// fenceCheck is the inline guard at the top of every public
// mutating / reading API. Returns ErrFenced if the namespace has
// observed a takeover; the caller should NOT proceed.
func fenceCheck(st *namespaceState) error {
	if st.fenced.Load() {
		return ErrFenced
	}
	return nil
}

func namespaceActiveCheck(st *namespaceState) error {
	if err := fenceCheck(st); err != nil {
		return err
	}
	if st.retired.Load() {
		return errNamespaceDropped
	}
	return nil
}
