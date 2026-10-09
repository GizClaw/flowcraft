package manager

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils/lock"
	"github.com/GizClaw/flowcraft/craft"
)

type testCapability struct{}

func (testCapability) Name() string { return "test" }

func (testCapability) Register(registry *resource.Registry) error {
	return event.Register(registry)
}

const definition = `craft: {id: test, version: 0.1.0}
deploy:
  version: v1
  resources:
    bus: {kind: event.Bus, impl: memory}
  runtime:
    event_bus: bus
`

func writeDefinition(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "craft.yaml")
	if err := os.WriteFile(path, []byte(definition), 0o600); err != nil {
		t.Fatalf("write craft.yaml: %v", err)
	}
	return path
}

// newTestManager builds a locked manager over dataDir, tuned by the
// caller.
func newTestManager(
	t *testing.T, definitionPath, dataDir string, tune func(*Options),
) *Manager {
	t.Helper()
	opts := Options{
		DefinitionPath: definitionPath,
		Paths:          Paths{DataDir: dataDir},
		Lock:           true,
		Capabilities:   []craft.Capability{testCapability{}},
	}
	if tune != nil {
		tune(&opts)
	}
	manager, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return manager
}

func TestLocateDefinition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	located, err := LocateDefinition("", func(string) (string, bool) {
		return "", false
	}, dir, "")
	if err != nil || located != path {
		t.Fatalf("LocateDefinition = %q, %v; want %q", located, err, path)
	}
	if _, err := LocateDefinition("", nil, t.TempDir(), ""); err == nil {
		t.Fatal("missing definition did not error")
	}
}

func TestManagerLifecycleAndLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	opts := Options{
		DefinitionPath: path,
		Paths:          Paths{DataDir: dir},
		Lock:           true,
		Capabilities:   []craft.Capability{testCapability{}},
	}
	m, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m.State() != StateRunning || m.Craft() == nil {
		t.Fatalf("state=%s craft=%v", m.State(), m.Craft())
	}
	second, err := New(opts)
	if err != nil {
		t.Fatalf("New second: %v", err)
	}
	if err := second.Start(context.Background()); err == nil {
		t.Fatal("second manager acquired the same lock")
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.State() != StateStopped {
		t.Fatalf("state = %s, want stopped", m.State())
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("restart after release: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type nilRunner struct{ called bool }

func (r *nilRunner) Run(context.Context, *craft.Craft) error {
	r.called = true
	return nil
}

func TestManagerSingleInstanceLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	ctx := context.Background()
	first := newTestManager(t, path, dir, nil)
	if err := first.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	var seen []lock.Info
	second := newTestManager(t, path, dir, func(opts *Options) {
		opts.OnLockHeld = func(info lock.Info) error {
			seen = append(seen, info)
			return nil
		}
	})
	err := second.Start(ctx)
	if err == nil {
		t.Fatal("second manager started on a held lock")
	}
	if !errdefs.IsConflict(err) {
		t.Fatalf("error = %v, want a conflict", err)
	}
	// The holder travels with the error, so a shell never has to guess who
	// owns the DataDir.
	holder, held := lock.IsHeld(err)
	if !held {
		t.Fatalf("IsHeld(%v) = false", err)
	}
	if holder.PID != os.Getpid() || holder.Kind != lockKind {
		t.Fatalf("holder = %+v, want this process and kind %s", holder, lockKind)
	}
	if len(seen) != 1 || seen[0].PID != os.Getpid() || seen[0].Kind != lockKind {
		t.Fatalf("OnLockHeld saw %+v, want one record for this process", seen)
	}
	if second.State() != StateFailed {
		t.Fatalf("state = %s, want failed", second.State())
	}

	// The hook chooses how the launch ends: its error replaces the default
	// conflict (this is where a shell forwards arguments to the holder and
	// reports "already running").
	alreadyRunning := errors.New("craft manager: forwarded to the running instance")
	third := newTestManager(t, path, dir, func(opts *Options) {
		opts.OnLockHeld = func(lock.Info) error { return alreadyRunning }
	})
	hookErr := third.Start(ctx)
	if !errors.Is(hookErr, alreadyRunning) {
		t.Fatalf("Start = %v, want the hook's error", hookErr)
	}
	if _, held := lock.IsHeld(hookErr); held {
		t.Fatal("the hook's error still carries the lock's HeldError")
	}

	// The record is on disk and readable without attempting the lock.
	recorded, ok := lock.ReadInfo(lockPath(dir, ""))
	if !ok || recorded.PID != os.Getpid() || recorded.Kind != lockKind {
		t.Fatalf("record = %+v (ok=%v), want this process", recorded, ok)
	}

	// Releasing the first instance frees the file for the next launch.
	if err := first.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	fourth := newTestManager(t, path, dir, nil)
	if err := fourth.Start(ctx); err != nil {
		t.Fatalf("start after release: %v", err)
	}
	if err := fourth.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestManagerProfileLocksAreSeparate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	ctx := context.Background()
	// Two profiles share one DataDir and must not lock each other out; the
	// profile is part of the lock file's name.
	defaultManager := newTestManager(t, path, dir, nil)
	if err := defaultManager.Start(ctx); err != nil {
		t.Fatalf("Start default profile: %v", err)
	}
	t.Cleanup(func() { _ = defaultManager.Close() })
	work := newTestManager(t, path, dir, func(opts *Options) {
		opts.Profile = "work"
	})
	if err := work.Start(ctx); err != nil {
		t.Fatalf("Start work profile: %v", err)
	}
	if err := work.Close(); err != nil {
		t.Fatalf("Close work profile: %v", err)
	}
	if want := filepath.Join(dir, "craft-work.lock"); lockPath(dir, "work") != want {
		t.Fatalf("lockPath(work) = %q, want %q", lockPath(dir, "work"), want)
	}
	// Released means acquirable again, not "file deleted": the record stays
	// behind on purpose.
	reopened := newTestManager(t, path, dir, func(opts *Options) {
		opts.Profile = "work"
	})
	if err := reopened.Start(ctx); err != nil {
		t.Fatalf("re-start the work profile after release: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close reopened work profile: %v", err)
	}
}

func TestManagerLockTakesOverLegacyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	legacy := lockPath(dir, "")
	// The O_EXCL manager left a bare pid line behind. Nobody holds a kernel
	// lock on it, so the launch takes it over with no cleanup step.
	if err := os.WriteFile(legacy, []byte("4242\n"), 0o600); err != nil {
		t.Fatalf("write legacy lock: %v", err)
	}
	if info, ok := lock.ReadInfo(legacy); ok {
		t.Fatalf("legacy record read as %+v, want unreadable", info)
	}
	manager := newTestManager(t, path, dir, nil)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("start over a legacy lock file: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	recorded, ok := lock.ReadInfo(legacy)
	if !ok || recorded.PID != os.Getpid() || recorded.Kind != lockKind {
		t.Fatalf("record = %+v (ok=%v), want this process", recorded, ok)
	}
}

func TestManagerLockUnavailable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	ctx := context.Background()
	// A directory where the lock file belongs cannot be locked, and it is
	// not "held by someone". The launch proceeds with a warning: a lock
	// that cannot be taken must not keep the user out.
	if err := os.Mkdir(lockPath(dir, ""), 0o700); err != nil {
		t.Fatalf("mkdir lock path: %v", err)
	}
	manager := newTestManager(t, path, dir, nil)
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start without a usable lock: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Refusing instead is the caller's call, through OnLockUnavailable.
	refused := errors.New("craft manager: refusing to start without the lock")
	var unavailable error
	strict := newTestManager(t, path, dir, func(opts *Options) {
		opts.OnLockUnavailable = func(err error) error {
			unavailable = err
			return refused
		}
	})
	if err := strict.Start(ctx); !errors.Is(err, refused) {
		t.Fatalf("Start = %v, want the hook's refusal", err)
	}
	if unavailable == nil {
		t.Fatal("OnLockUnavailable was not consulted")
	}
	if _, held := lock.IsHeld(unavailable); held {
		t.Fatalf("unusable lock reported as held: %v", unavailable)
	}
	if strict.State() != StateFailed {
		t.Fatalf("state = %s, want failed", strict.State())
	}
}

const (
	holderFlagEnv    = "CRAFT_MANAGER_TEST_HOLDER"
	holderPathEnv    = "CRAFT_MANAGER_TEST_PATH"
	holderDataDirEnv = "CRAFT_MANAGER_TEST_DATA_DIR"
)

// TestHelperManagerHolder is not a test: it is the child half of the crash
// case. The parent re-executes this test binary with the holder flag set,
// the child starts a real Manager (single-instance lock included), prints
// "locked" and blocks until the parent kills it — which is the moment the
// kernel must release the lock.
func TestHelperManagerHolder(t *testing.T) {
	if os.Getenv(holderFlagEnv) != "1" {
		return
	}
	manager, err := New(Options{
		DefinitionPath: os.Getenv(holderPathEnv),
		Paths:          Paths{DataDir: os.Getenv(holderDataDirEnv)},
		Lock:           true,
		Capabilities:   []craft.Capability{testCapability{}},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "holder new:", err)
		os.Exit(2)
	}
	if err := manager.Start(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "holder start:", err)
		os.Exit(2)
	}
	fmt.Println("locked")
	// Sleep instead of blocking on an empty select: the runtime's deadlock
	// detector would abort the holder before the parent's assertions run.
	for {
		time.Sleep(time.Hour)
	}
}

func TestManagerStartsAfterHolderCrash(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	child, stop := startManagerHolder(t, dir, path)
	defer stop()

	// While the holder lives, the launch is refused and the conflict names
	// it instead of leaving the user with a locked DataDir.
	second := newTestManager(t, path, dir, nil)
	err := second.Start(context.Background())
	holder, held := lock.IsHeld(err)
	if !held {
		t.Fatalf("Start = %v, want a held lock", err)
	}
	if holder.PID != child.Process.Pid {
		t.Fatalf("holder = %+v, want the child pid %d", holder, child.Process.Pid)
	}

	// kill -9 is the case the old O_EXCL lock could not survive: the next
	// instance starts with no cleanup and no staleness heuristic.
	stop()
	takeover := newTestManager(t, path, dir, nil)
	if err := takeover.Start(context.Background()); err != nil {
		t.Fatalf("start after the holder was killed: %v", err)
	}
	t.Cleanup(func() { _ = takeover.Close() })
	if takeover.State() != StateRunning {
		t.Fatalf("state = %s, want running", takeover.State())
	}
}

// startManagerHolder re-executes this test binary as a Manager holding the
// lock on dataDir and waits until it reports the lock is taken.
func startManagerHolder(t *testing.T, dataDir, definitionPath string) (*exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperManagerHolder")
	cmd.Env = append(os.Environ(),
		holderFlagEnv+"=1",
		holderPathEnv+"="+definitionPath,
		holderDataDirEnv+"="+dataDir,
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
	case <-time.After(60 * time.Second):
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

func TestManagerRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	m, err := New(Options{
		DefinitionPath: path,
		Paths:          Paths{DataDir: dir},
		Capabilities:   []craft.Capability{testCapability{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runner := &nilRunner{}
	if err := m.Run(context.Background(), runner); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !runner.called || m.State() != StateStopped {
		t.Fatalf("runner called=%v state=%s", runner.called, m.State())
	}
}

func TestManagerCloseReleasesLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	ctx := context.Background()
	// Close is the final stop, so it has to release the lock: an
	// unstopped manager would keep a live holder on the DataDir and the
	// next launch could never start.
	closed := newTestManager(t, path, dir, nil)
	if err := closed.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closed.State() != StateStopped {
		t.Fatalf("state = %s, want stopped", closed.State())
	}
	if err := closed.Start(ctx); !errdefs.IsNotAvailable(err) {
		t.Fatalf("restart of a closed manager = %v, want not available", err)
	}
	next := newTestManager(t, path, dir, nil)
	if err := next.Start(ctx); err != nil {
		t.Fatalf("start after Close: %v", err)
	}
	if err := next.Close(); err != nil {
		t.Fatalf("Close next: %v", err)
	}
}

// TestManagerDrainReturnsToRunning covers the state a drain leaves
// behind: it waits, it does not stop, so the instance must still be the
// running one afterwards — otherwise Replace, which requires a running
// Craft, could never follow a drain.
func TestManagerDrainReturnsToRunning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDefinition(t, dir)
	ctx := context.Background()
	m := newTestManager(t, path, dir, nil)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	before := m.Craft()
	if err := m.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if m.State() != StateRunning {
		t.Fatalf("state after Drain = %s, want running", m.State())
	}
	if m.Craft() != before {
		t.Fatal("a drain replaced the Craft")
	}

	def, err := craft.ParseDefinition([]byte(definition))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	if err := m.Replace(ctx, def); err != nil {
		t.Fatalf("Replace after a drain: %v", err)
	}
	if m.State() != StateRunning || m.Craft() == nil {
		t.Fatalf("after Replace state=%s craft=%v", m.State(), m.Craft())
	}
	// The retired instance is closed, not merely dropped: a Craft left
	// behind keeps its goroutines, its plugin processes and its endpoints
	// alive with nothing left to reach it.
	if err := before.Start(ctx); !errors.Is(err, craft.ErrCraftClosed) {
		t.Fatalf("the retired Craft answers Start with %v, want ErrCraftClosed",
			err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
