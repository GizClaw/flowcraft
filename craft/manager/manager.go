// Package manager owns the process-level lifecycle of one Craft:
// definition lookup, profiles and paths, the single-instance lock,
// migrations, the state machine, signal-aware Run, and the blue-green
// Replace used when a change needs a fresh Craft.
package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/utils/lock"
	"github.com/GizClaw/flowcraft/craft"
)

// State is the manager lifecycle state.
type State string

const (
	StateIdle     State = "idle"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateDraining State = "draining"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
)

// RestartPolicy bounds automatic restarts after a runner failure. Max
// <= 0 disables restarts.
type RestartPolicy struct {
	Max     int
	Backoff time.Duration
}

// Paths are the application roots of one manager instance.
type Paths struct {
	ConfigDir string
	DataDir   string
	AppHome   string
}

// Runner is the shell entry point (desktop, headless, http). Run blocks
// until the shell exits or ctx is cancelled.
type Runner interface {
	Run(ctx context.Context, c *craft.Craft) error
}

// Options configures a Manager.
type Options struct {
	// DefinitionPath is the explicit craft.yaml path; empty triggers
	// the standard lookup order in LocateDefinition.
	DefinitionPath string
	Profile        string
	Paths          Paths
	// Lock enables the DataDir+Profile single-instance lock.
	Lock bool
	// OnLockHeld is consulted when another live instance already holds the
	// lock. The holder record arrives as lock.Info (pid, kind, start,
	// version, optional endpoint), so a shell can forward its arguments to
	// the running instance and raise it — the record's endpoint is where
	// that instance can be reached. The returned error is what Start
	// reports; nil keeps the default conflict error. The hook cannot let a
	// second instance proceed: the lock is what makes the launch single.
	OnLockHeld func(lock.Info) error
	// OnLockUnavailable is consulted when the lock cannot be taken at all —
	// a filesystem that will not lock, a directory where the lock file
	// belongs, missing permissions. That is the state the lock cannot
	// answer for, so the default is to start without it and log a warning:
	// a lock that cannot be taken must not keep the user out. Return an
	// error to refuse the launch instead.
	OnLockUnavailable func(error) error
	// Migrate runs before the Craft is built.
	Migrate func(ctx context.Context, paths Paths) error
	// Restart bounds automatic restarts in Run.
	Restart RestartPolicy
	// Capabilities and Build choose how the Craft is constructed. When
	// Build is nil, craft.New is called with Capabilities.
	Capabilities []craft.Capability
	Build        func(ctx context.Context, def craft.Definition, paths Paths) (*craft.Craft, error)
}

// Manager owns one Craft and its process lifecycle.
type Manager struct {
	opts Options

	mu      sync.Mutex
	state   State
	current *craft.Craft
	lock    *lock.Handle
	subs    map[uint64]func(State)
	nextSub uint64
	closed  bool
}

// New validates the options and returns an idle manager.
func New(opts Options) (*Manager, error) {
	if opts.Build == nil && len(opts.Capabilities) == 0 {
		return nil, errdefs.Validationf(
			"craft manager: Build or Capabilities is required")
	}
	if opts.Lock && strings.TrimSpace(opts.Paths.DataDir) == "" {
		return nil, errdefs.Validationf(
			"craft manager: Lock requires Paths.DataDir")
	}
	if opts.Restart.Backoff <= 0 {
		opts.Restart.Backoff = time.Second
	}
	return &Manager{opts: opts, state: StateIdle, subs: map[uint64]func(State){}}, nil
}

// State returns the current state.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Craft returns the current Craft, or nil before Start.
func (m *Manager) Craft() *craft.Craft {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Subscribe observes state transitions.
func (m *Manager) Subscribe(fn func(State)) func() {
	if fn == nil {
		return func() {}
	}
	m.mu.Lock()
	m.nextSub++
	id := m.nextSub
	m.subs[id] = fn
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.subs, id)
		m.mu.Unlock()
	}
}

// notifyState invokes the state subscribers after the state mutation,
// outside the lock: subscribers may call back into the manager.
func (m *Manager) notifyState(state State) {
	m.mu.Lock()
	subs := make([]func(State), 0, len(m.subs))
	for _, fn := range m.subs {
		subs = append(subs, fn)
	}
	m.mu.Unlock()
	for _, fn := range subs {
		fn(state)
	}
}

// Start loads the definition, migrates, locks, builds and starts the
// Craft. It is idempotent while running.
func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		return errdefs.Validationf("craft manager: context is required")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errdefs.NotAvailablef("craft manager: closed")
	}
	if m.state == StateRunning {
		m.mu.Unlock()
		return nil
	}
	if m.state == StateStarting || m.state == StateStopping ||
		m.state == StateDraining {
		m.mu.Unlock()
		return errdefs.Conflictf(
			"craft manager: state %s is busy", m.state)
	}
	m.state = StateStarting
	m.mu.Unlock()
	m.notifyState(StateStarting)

	fail := func(err error) error {
		m.mu.Lock()
		m.state = StateFailed
		m.mu.Unlock()
		m.notifyState(StateFailed)
		return err
	}
	path := m.opts.DefinitionPath
	if strings.TrimSpace(path) == "" {
		located, err := LocateDefinition("", os.LookupEnv, mustGetwd(), executableDir())
		if err != nil {
			return fail(err)
		}
		path = located
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fail(errdefs.Validationf(
			"craft manager: read definition: %v", err))
	}
	def, err := craft.ParseDefinition(raw)
	if err != nil {
		return fail(err)
	}
	if m.opts.Migrate != nil {
		if err := m.opts.Migrate(ctx, m.opts.Paths); err != nil {
			return fail(fmt.Errorf("craft manager: migrate: %w", err))
		}
	}
	if m.opts.Lock {
		handle, err := m.acquireLock(ctx)
		if err != nil {
			return fail(err)
		}
		if handle != nil {
			m.mu.Lock()
			m.lock = handle
			m.mu.Unlock()
		}
	}
	built, err := m.build(ctx, def)
	if err != nil {
		m.releaseLock()
		return fail(err)
	}
	if err := built.Start(ctx); err != nil {
		_ = built.Close()
		m.releaseLock()
		return fail(err)
	}
	m.mu.Lock()
	m.current = built
	m.state = StateRunning
	m.mu.Unlock()
	m.notifyState(StateRunning)
	_ = built.Emit(ctx, craft.SubjectManagerState, craft.ManagerStateEvent{
		State: string(StateRunning),
	})
	return nil
}

func (m *Manager) build(
	ctx context.Context,
	def craft.Definition,
) (*craft.Craft, error) {
	if m.opts.Build != nil {
		return m.opts.Build(ctx, def, m.opts.Paths)
	}
	return craft.New(def, craft.Options{
		ConfigDir:     m.opts.Paths.ConfigDir,
		DataDir:       m.opts.Paths.DataDir,
		AppHome:       m.opts.Paths.AppHome,
		DefinitionDir: filepath.Dir(m.opts.DefinitionPath),
		Capabilities:  m.opts.Capabilities,
	})
}

// Run starts the Craft, runs the shell runner, then stops. Runner
// failures are retried per RestartPolicy.
func (m *Manager) Run(ctx context.Context, runner Runner) error {
	if runner == nil {
		return errdefs.Validationf("craft manager: runner is required")
	}
	attempts := m.opts.Restart.Max
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := m.Start(ctx); err != nil {
			return err
		}
		instance := m.Craft()
		lastErr = runner.Run(ctx, instance)
		if ctx.Err() != nil {
			break
		}
		if attempt >= attempts || m.opts.Restart.Max <= 0 {
			break
		}
		select {
		case <-time.After(m.opts.Restart.Backoff):
		case <-ctx.Done():
		}
	}
	stopErr := m.Stop(context.WithoutCancel(ctx))
	return errors.Join(lastErr, stopErr)
}

// Replace builds a new Craft with the same paths, starts it, swaps it
// in, and retires the previous instance.
func (m *Manager) Replace(
	ctx context.Context,
	def craft.Definition,
) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errdefs.NotAvailablef("craft manager: closed")
	}
	previous := m.current
	if previous == nil || m.state != StateRunning {
		m.mu.Unlock()
		return errdefs.Conflictf(
			"craft manager: Replace requires a running Craft")
	}
	m.mu.Unlock()
	built, err := m.build(ctx, def)
	if err != nil {
		return err
	}
	if err := built.Start(ctx); err != nil {
		_ = built.Close()
		return err
	}
	m.mu.Lock()
	m.current = built
	m.state = StateRunning
	m.mu.Unlock()
	m.notifyState(StateRunning)
	// The retired instance is closed whatever the drain does: the manager
	// already points at the new one, so a previous instance left behind
	// keeps its goroutines, plugin processes and lock file alive with
	// nothing left to reach it.
	drainErr := previous.Drain(ctx)
	closeErr := previous.Close()
	if drainErr != nil || closeErr != nil {
		return fmt.Errorf("craft manager: retire previous instance: %w",
			errors.Join(drainErr, closeErr))
	}
	return nil
}

// Drain waits for in-flight work to finish. The instance stays live and
// the manager goes back to running once it has.
func (m *Manager) Drain(ctx context.Context) error {
	m.mu.Lock()
	current := m.current
	if current != nil {
		m.state = StateDraining
	}
	m.mu.Unlock()
	if current == nil {
		return nil
	}
	m.notifyState(StateDraining)
	err := current.Drain(ctx)
	// A drain is a wait, not a stop, so the state machine returns to
	// running; without that a drain would park the manager in
	// StateDraining forever and Replace — which requires a running
	// instance — could never follow one. A Stop or a Replace that got
	// there first wins: the state is only restored while this call's
	// instance is still the current one.
	m.mu.Lock()
	restored := m.current == current && m.state == StateDraining
	if restored {
		m.state = StateRunning
	}
	m.mu.Unlock()
	if restored {
		m.notifyState(StateRunning)
	}
	return err
}

// Stop drains and closes the Craft, releasing the lock.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	current := m.current
	m.current = nil
	if current == nil {
		m.state = StateStopped
		m.mu.Unlock()
		m.notifyState(StateStopped)
		return nil
	}
	m.state = StateStopping
	m.mu.Unlock()
	m.notifyState(StateStopping)
	_ = current.Emit(ctx, craft.SubjectManagerState, craft.ManagerStateEvent{
		State: string(StateStopping),
	})
	var errs []error
	if err := current.Drain(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := current.Close(); err != nil {
		errs = append(errs, err)
	}
	m.releaseLock()
	m.mu.Lock()
	m.state = StateStopped
	m.mu.Unlock()
	m.notifyState(StateStopped)
	return errors.Join(errs...)
}

// Close is Stop with a background timeout, and it is final: a closed
// manager has released its lock and refuses to start again. The flag is
// set after the stop, not before it: Stop short-circuits on the flag, so
// marking the manager closed first would turn Close into a no-op that
// leaves the Craft running and the single-instance lock held.
func (m *Manager) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := m.Stop(ctx)
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return err
}

func (m *Manager) releaseLock() {
	m.mu.Lock()
	handle := m.lock
	m.lock = nil
	m.mu.Unlock()
	if handle != nil {
		_ = handle.Release()
	}
}

// LocateDefinition resolves craft.yaml: explicit, $CRAFT_DEFINITION,
// the working directory, the executable directory, then the user
// config directory.
func LocateDefinition(
	explicit string,
	lookup func(string) (string, bool),
	workingDir, execDir string,
) (string, error) {
	candidates := make([]string, 0, 5)
	if strings.TrimSpace(explicit) != "" {
		candidates = append(candidates, explicit)
	}
	if lookup != nil {
		if value, ok := lookup("CRAFT_DEFINITION"); ok {
			candidates = append(candidates, value)
		}
	}
	if workingDir != "" {
		candidates = append(candidates, filepath.Join(workingDir, "craft.yaml"))
	}
	if execDir != "" {
		candidates = append(candidates, filepath.Join(execDir, "craft.yaml"))
	}
	if dir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "craft", "craft.yaml"))
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errdefs.NotFoundf("craft manager: craft.yaml not found")
}

func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}

func executableDir() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(path)
}

// lockKind is the holder kind recorded in the single-instance lock file.
const lockKind = "manager"

// lockPath resolves the DataDir+Profile lock file: craft.lock, or
// craft-<profile>.lock for a named profile.
func lockPath(dataDir, profile string) string {
	name := "craft.lock"
	if strings.TrimSpace(profile) != "" {
		name = "craft-" + profile + ".lock"
	}
	return filepath.Join(dataDir, name)
}

// acquireLock takes the single-instance lock of this manager's DataDir and
// profile.
//
// A live holder is the one outcome that refuses the launch: OnLockHeld gets
// the holder record (and may raise the running instance, forward arguments)
// and chooses the error Start reports. Anything else — an unusable path, a
// filesystem without lock semantics — is a question the lock cannot answer,
// so the manager starts without it after a warning, unless OnLockUnavailable
// refuses. A nil handle means exactly that: no lock was taken, start anyway.
func (m *Manager) acquireLock(ctx context.Context) (*lock.Handle, error) {
	path := lockPath(m.opts.Paths.DataDir, m.opts.Profile)
	handle, err := lock.Acquire(ctx, path, lockKind)
	if err == nil {
		return handle, nil
	}
	if holder, held := lock.IsHeld(err); held {
		if m.opts.OnLockHeld != nil {
			if refusal := m.opts.OnLockHeld(holder); refusal != nil {
				return nil, refusal
			}
		}
		return nil, errdefs.Conflictf(
			"craft manager: another instance holds %s: %w", path, err)
	}
	if m.opts.OnLockUnavailable != nil {
		if refusal := m.opts.OnLockUnavailable(err); refusal != nil {
			return nil, refusal
		}
	}
	telemetry.Warn(ctx, fmt.Sprintf(
		"craft manager: single-instance lock unavailable, starting without it: %v",
		err))
	return nil, nil
}
