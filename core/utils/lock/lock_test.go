package lock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	helperEnv     = "LOCK_TEST_HELPER"
	helperPathEnv = "LOCK_TEST_PATH"
	helperKindEnv = "LOCK_TEST_KIND"
)

// TestHelperProcess is not a test: it is the child half of the
// cross-process cases. The parent re-executes this test binary with
// LOCK_TEST_HELPER=1, the child takes the lock, prints "locked" and
// blocks until the parent kills it — which is the moment the kernel must
// release the lock.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	handle, err := Acquire(context.Background(),
		os.Getenv(helperPathEnv), os.Getenv(helperKindEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper acquire:", err)
		os.Exit(2)
	}
	_ = handle
	fmt.Println("locked")
	// Sleep instead of blocking on an empty select: the runtime's
	// deadlock detector would abort the holder before the parent's
	// assertions run.
	for {
		time.Sleep(time.Hour)
	}
}

func TestAcquireReleaseRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")
	handle, err := Acquire(context.Background(), path, "manager")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !handle.Owned() {
		t.Fatal("first handle is not the owner")
	}
	if handle.Path() != path {
		t.Fatalf("Path = %q, want %q", handle.Path(), path)
	}
	if info := handle.Info(); info.PID != os.Getpid() || info.Kind != "manager" {
		t.Fatalf("holder info = %+v, want this process and kind manager", info)
	}
	recorded, ok := ReadInfo(path)
	if !ok || recorded.PID != os.Getpid() {
		t.Fatalf("recorded holder = %+v (ok=%v), want this process", recorded, ok)
	}
	if err := handle.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	// The file keeps the last holder's record; only the kernel lock
	// decides liveness, so the path must be acquirable again right away.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file did not survive release: %v", err)
	}
	again, err := Acquire(context.Background(), path, "host")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if !again.Owned() {
		t.Fatal("re-acquire returned a shared handle")
	}
	// ReadInfo reports the last writer, not the first.
	if last, ok := ReadInfo(path); !ok || last.Kind != "host" {
		t.Fatalf("record after re-acquire = %+v (ok=%v), want kind host", last, ok)
	}
	if err := again.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

func TestSecondAcquireInOneProcessIsHeld(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")
	owner, err := Acquire(context.Background(), path, "manager")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = owner.Release() }()

	// The default is strict: two managers over one DataDir are two
	// instances even when they share a process.
	handle, err := Acquire(context.Background(), path, "manager")
	if handle != nil || err == nil {
		t.Fatalf("strict second acquire = (%v, %v), want held", handle, err)
	}
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("error = %v, want ErrHeld", err)
	}
	holder, ok := IsHeld(err)
	if !ok {
		t.Fatalf("IsHeld(%v) = false", err)
	}
	if holder.PID != os.Getpid() || holder.Kind != "manager" {
		t.Fatalf("holder = %+v, want this process and the owner's kind", holder)
	}
	if start, parseErr := time.Parse(time.RFC3339, holder.Started); parseErr != nil {
		t.Fatalf("holder started = %q: %v", holder.Started, parseErr)
	} else if time.Since(start) > time.Minute {
		t.Fatalf("holder started %s in the past", holder.Started)
	}
}

func TestShareWithinProcess(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")
	owner, err := Acquire(context.Background(), path, "host", ShareWithinProcess())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	shared, err := Acquire(context.Background(), path, "recovery", ShareWithinProcess())
	if err != nil {
		t.Fatalf("shared acquire: %v", err)
	}
	if shared.Owned() {
		t.Fatal("shared handle claims ownership")
	}
	if info := shared.Info(); info.PID != os.Getpid() || info.Kind != "host" {
		t.Fatalf("shared info = %+v, want the owner's record", info)
	}
	// Releasing the shared handle is a no-op: the file stays locked until
	// the owning handle lets go.
	if err := shared.Release(); err != nil {
		t.Fatalf("shared release: %v", err)
	}
	if _, err := Acquire(context.Background(), path, "host"); err == nil {
		t.Fatal("shared release dropped the owner's lock")
	}
	if err := owner.Release(); err != nil {
		t.Fatalf("owner release: %v", err)
	}
	after, err := Acquire(context.Background(), path, "host")
	if err != nil {
		t.Fatalf("acquire after owner release: %v", err)
	}
	if err := after.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestAcquireReportsLiveForeignHolder(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")
	child, stop := startHolder(t, path, "host")
	defer stop()

	handle, err := Acquire(context.Background(), path, "manager")
	if handle != nil || err == nil {
		t.Fatalf("acquire while a live process holds it = (%v, %v), want held",
			handle, err)
	}
	holder, ok := IsHeld(err)
	if !ok {
		t.Fatalf("IsHeld(%v) = false", err)
	}
	if holder.PID != child.Process.Pid || holder.Kind != "host" {
		t.Fatalf("holder = %+v, want pid %d kind host", holder, child.Process.Pid)
	}
	if info, ok := ReadInfo(path); !ok || info.PID != child.Process.Pid {
		t.Fatalf("ReadInfo = %+v (ok=%v), want the child's record", info, ok)
	}

	// Killing the holder is what the whole design leans on: the kernel
	// drops the lock, and the next process takes the file without anyone
	// cleaning up a thing.
	stop()
	recovered, err := Acquire(context.Background(), path, "manager")
	if err != nil {
		t.Fatalf("acquire after the holder died: %v", err)
	}
	if !recovered.Owned() {
		t.Fatal("handle after the holder died is not the owner")
	}
	// The stale record is rewritten by the new holder.
	if info, ok := ReadInfo(path); !ok || info.PID != os.Getpid() {
		t.Fatalf("record after takeover = %+v (ok=%v), want this process", info, ok)
	}
	if err := recovered.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestAcquireFailsOnUnusablePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A directory can never be a lock file; the error must not read as
	// "someone else holds it", or callers would refuse work forever.
	handle, err := Acquire(context.Background(), dir, "manager")
	if err == nil {
		t.Fatalf("acquiring a directory path succeeded: %v", handle)
	}
	if _, held := IsHeld(err); held {
		t.Fatalf("directory error reported as held: %v", err)
	}
	// Neither can a path whose parent is a file.
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := Acquire(context.Background(), filepath.Join(file, "nested.lock"), "manager"); err == nil {
		t.Fatal("acquiring under a file succeeded")
	} else if _, held := IsHeld(err); held {
		t.Fatalf("nested-path error reported as held: %v", err)
	}
}

func TestAcquireTakesOverForeignRecords(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// A record from a process that is gone: readable, not authoritative.
	jsonPath := filepath.Join(dir, "json.lock")
	if err := os.WriteFile(jsonPath, []byte(`{"pid":999999,"kind":"manager"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
	if info, ok := ReadInfo(jsonPath); !ok || info.PID != 999999 {
		t.Fatalf("ReadInfo = %+v (ok=%v), want the stale record", info, ok)
	}
	handle, err := Acquire(context.Background(), jsonPath, "manager")
	if err != nil {
		t.Fatalf("acquire over a stale record: %v", err)
	}
	if err := handle.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	// A record in the pre-lock format (the bare pid line the O_EXCL
	// manager wrote) is unreadable, not fatal: the lock takes over and the
	// next writer replaces it.
	legacyPath := filepath.Join(dir, "legacy.lock")
	if err := os.WriteFile(legacyPath, []byte("4242\n"), 0o600); err != nil {
		t.Fatalf("write legacy record: %v", err)
	}
	if info, ok := ReadInfo(legacyPath); ok {
		t.Fatalf("ReadInfo on a legacy record = %+v, want unreadable", info)
	}
	taken, err := Acquire(context.Background(), legacyPath, "manager")
	if err != nil {
		t.Fatalf("acquire over a legacy record: %v", err)
	}
	if info, ok := ReadInfo(legacyPath); !ok || info.PID != os.Getpid() {
		t.Fatalf("record after takeover = %+v (ok=%v), want this process", info, ok)
	}
	if err := taken.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestConcurrentAcquireHasOneOwner(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")
	const racers = 8
	var owners atomic.Int64
	var held atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			handle, err := Acquire(context.Background(), path, "manager")
			if err == nil {
				owners.Add(1)
				_ = handle.Release()
				return
			}
			if _, ok := IsHeld(err); !ok {
				t.Errorf("racer failed with %v, want held or owned", err)
				return
			}
			held.Add(1)
		}()
	}
	close(start)
	wg.Wait()
	if owners.Load()+held.Load() != racers {
		t.Fatalf("owners=%d held=%d, want %d decisions",
			owners.Load(), held.Load(), racers)
	}
	if owners.Load() == 0 {
		t.Fatal("no racer acquired the lock")
	}
	free, err := Acquire(context.Background(), path, "manager")
	if err != nil {
		t.Fatalf("acquire after the race: %v", err)
	}
	if err := free.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestAcquireOptionsAreRecorded(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.lock")
	handle, err := Acquire(context.Background(), path, "gui",
		WithVersion("1.2.3"), WithEndpoint("unix:///tmp/gui.sock"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = handle.Release() }()
	recorded, ok := ReadInfo(path)
	if !ok {
		t.Fatal("record is unreadable")
	}
	if recorded.Version != "1.2.3" || recorded.Endpoint != "unix:///tmp/gui.sock" {
		t.Fatalf("record = %+v, want the configured version and endpoint", recorded)
	}
	if _, err := Acquire(context.Background(), "", "manager"); err == nil {
		t.Fatal("empty path acquired")
	}
}

func TestInfoLiveFrom(t *testing.T) {
	t.Parallel()
	started := time.Now().UTC().Truncate(time.Second)
	info := Info{Started: started.Format(time.RFC3339)}
	if !info.LiveFrom(started) {
		t.Fatal("LiveFrom refused the holder's own start time")
	}
	if info.LiveFrom(started.Add(-time.Second)) {
		t.Fatal("LiveFrom accepted a deadline before the start time")
	}
	if (Info{}).LiveFrom(started) {
		t.Fatal("an empty record reported liveness")
	}
	if (Info{Started: "yesterday"}).LiveFrom(started) {
		t.Fatal("an unparseable start time reported liveness")
	}
}

// startHolder re-executes this test binary as a lock holder and waits
// until it reports the lock is taken.
func startHolder(t *testing.T, path, kind string) (*exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(),
		helperEnv+"=1",
		helperPathEnv+"="+path,
		helperKindEnv+"="+kind,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil {
			ready <- fmt.Errorf("holder exited before locking: %w", err)
			return
		}
		if line != "locked\n" {
			ready <- fmt.Errorf("holder said %q, want locked", line)
			return
		}
		ready <- nil
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("timed out waiting for the lock holder")
	}
	stopped := false
	return cmd, func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
