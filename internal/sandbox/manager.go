package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	gobreaker "github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ManagerConfig configures the sandbox Manager.
type ManagerConfig struct {
	Mode          Mode          `json:"mode,omitempty"`
	ExecTimeout   time.Duration `json:"exec_timeout,omitempty"`
	IdleTimeout   time.Duration `json:"idle_timeout,omitempty"`
	MaxConcurrent int           `json:"max_concurrent,omitempty"`
	RootDir       string        `json:"root_dir,omitempty"`
	Mounts        []MountConfig `json:"mounts,omitempty"`
	NetworkMode   string        `json:"network_mode,omitempty"`

	// Circuit breaker configuration
	CBCapacity         uint32        `json:"cb_capacity,omitempty"`          // Max requests in half-open state
	CBInterval         time.Duration `json:"cb_interval,omitempty"`          // Cyclic period for circuit breaker
	CBTimeout          time.Duration `json:"cb_timeout,omitempty"`           // Time in open state before half-open
	CBFailureThreshold int           `json:"cb_failure_threshold,omitempty"` // Consecutive failures to trip
}

// Validate checks the configuration for invalid values.
func (c ManagerConfig) Validate() error {
	if c.ExecTimeout <= 0 {
		return fmt.Errorf("sandbox: ExecTimeout must be positive, got %v", c.ExecTimeout)
	}
	if c.IdleTimeout <= 0 {
		return fmt.Errorf("sandbox: IdleTimeout must be positive, got %v", c.IdleTimeout)
	}
	if c.MaxConcurrent <= 0 {
		return fmt.Errorf("sandbox: MaxConcurrent must be positive, got %d", c.MaxConcurrent)
	}
	return nil
}

// DefaultManagerConfig returns sensible defaults.
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		Mode:          ModeSession,
		ExecTimeout:   5 * time.Minute,
		IdleTimeout:   30 * time.Minute,
		MaxConcurrent: 10,
	}
}

type sandboxEntry struct {
	sb       Sandbox
	mode     Mode
	refCount int
	lastUsed time.Time
	rootDir  string
}

// Manager manages sandbox lifecycle by session ID.
// Thread-safe. Includes a reaper goroutine for idle timeout reclamation
// and a circuit breaker protecting container creation.
type Manager struct {
	ctx            context.Context
	cfg            ManagerConfig
	mu             sync.Mutex
	entries        map[string]*sandboxEntry
	overlay        *OverlayManager // nil when overlay is unavailable or unneeded
	cb             *gobreaker.CircuitBreaker[createResult]
	localIsolation probeResult // cached isolation probe, shared by all local sandboxes
	done           chan struct{}
	wg             sync.WaitGroup
	closeOnce      sync.Once
}

type createResult struct {
	sb      Sandbox
	rootDir string
}

// NewManager creates a Manager and starts the idle reaper.
// The ctx is stored for telemetry in background operations (reaper, circuit breaker callbacks).
func NewManager(ctx context.Context, cfg ManagerConfig) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	if cfg.Mode == "" {
		cfg.Mode = ModeSession
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = 5 * time.Minute
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	if cfg.RootDir == "" {
		dir, err := os.MkdirTemp("", "flowcraft-sandbox-*")
		if err != nil {
			return nil, fmt.Errorf("sandbox: create temp root: %w", err)
		}
		cfg.RootDir = dir
	}

	// Use configured circuit breaker values or defaults
	cbMaxRequests := cfg.CBCapacity
	if cbMaxRequests == 0 {
		cbMaxRequests = 2
	}
	cbInterval := cfg.CBInterval
	if cbInterval == 0 {
		cbInterval = 60 * time.Second
	}
	cbTimeout := cfg.CBTimeout
	if cbTimeout == 0 {
		cbTimeout = 30 * time.Second
	}
	cbFailureThreshold := uint32(cfg.CBFailureThreshold)
	if cbFailureThreshold == 0 {
		cbFailureThreshold = 3
	}

	cb := gobreaker.NewCircuitBreaker[createResult](gobreaker.Settings{
		Name:        "sandbox-create",
		MaxRequests: cbMaxRequests,
		Interval:    cbInterval,
		Timeout:     cbTimeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= cbFailureThreshold
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			telemetry.Warn(ctx, "sandbox circuit breaker state change",
				otellog.String("name", name),
				otellog.String("from", from.String()),
				otellog.String("to", to.String()))
		},
	})

	var overlayMgr *OverlayManager
	if hasOverlayMounts(cfg.Mounts) && OverlaySupported() {
		overlayDir := filepath.Join(cfg.RootDir, ".overlays")
		om, omErr := NewOverlayManager(overlayDir)
		if omErr != nil {
			telemetry.Warn(ctx, "overlay manager init failed, falling back to direct mount",
				otellog.String("error", omErr.Error()))
		} else {
			overlayMgr = om
		}
	}

	localIso, probeErr := probeIsolation()
	if probeErr != nil {
		return nil, probeErr
	}
	telemetry.Info(ctx, "sandbox: isolation probe complete",
		otellog.String("backend", localIso.backend.String()))

	m := &Manager{
		ctx:            ctx,
		cfg:            cfg,
		entries:        make(map[string]*sandboxEntry),
		overlay:        overlayMgr,
		cb:             cb,
		localIsolation: localIso,
		done:           make(chan struct{}),
	}
	m.wg.Add(1)
	go m.reaper()
	return m, nil
}

// Acquire returns an existing sandbox for runtimeID or creates a new one.
func (m *Manager) Acquire(ctx context.Context, runtimeID string, opts AcquireOptions) (Sandbox, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.acquire",
		trace.WithAttributes(
			attribute.String("sandbox.runtime_id", runtimeID),
		),
	)
	defer span.End()

	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.entries[runtimeID]; ok {
		e.refCount++
		e.lastUsed = time.Now()
		return e.sb, nil
	}

	if m.cfg.MaxConcurrent > 0 && len(m.entries) >= m.cfg.MaxConcurrent {
		return nil, fmt.Errorf("%w: %d", ErrLimitReached, m.cfg.MaxConcurrent)
	}

	mode := opts.Mode
	if mode == "" {
		mode = m.cfg.Mode
	}

	result, err := m.cb.Execute(func() (createResult, error) {
		sb, rootDir, err := m.create(ctx, runtimeID)
		if err != nil {
			return createResult{}, err
		}
		return createResult{sb: sb, rootDir: rootDir}, nil
	})
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("sandbox: create: %w", err)
	}

	m.entries[runtimeID] = &sandboxEntry{
		sb: result.sb, mode: mode, refCount: 1, lastUsed: time.Now(), rootDir: result.rootDir,
	}
	sbContainersActive.Add(ctx, 1)
	sbContainersCreated.Add(ctx, 1,
		metric.WithAttributes(attribute.String("mode", string(mode))))
	telemetry.Info(ctx, "sandbox acquired",
		otellog.String("runtime_id", runtimeID),
		otellog.String("mode", string(mode)))
	return result.sb, nil
}

// Release decrements the reference count. Ephemeral sandboxes are destroyed
// when count reaches zero.
func (m *Manager) Release(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.releaseLocked(sessionID)
}

func (m *Manager) releaseLocked(sessionID string) error {
	e, ok := m.entries[sessionID]
	if !ok {
		return nil
	}
	e.refCount--
	if e.refCount > 0 {
		return nil
	}
	if e.mode == ModeEphemeral {
		return m.destroyLocked(sessionID)
	}
	e.lastUsed = time.Now()
	return nil
}

// Close shuts down the Manager, destroying all sandboxes. Idempotent.
func (m *Manager) Close() error {
	var firstErr error
	m.closeOnce.Do(func() {
		close(m.done)
		m.wg.Wait()

		m.mu.Lock()
		defer m.mu.Unlock()

		for sid := range m.entries {
			if err := m.destroyLocked(sid); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

// Config returns a read-only copy of the manager configuration.
func (m *Manager) Config() ManagerConfig { return m.cfg }

// Stats returns the number of active sandboxes.
func (m *Manager) Stats() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

func (m *Manager) create(_ context.Context, sessionID string) (Sandbox, string, error) {
	rootDir := filepath.Join(m.cfg.RootDir, "local", sessionID)
	specs := []SymlinkSpec{
		{Name: "skills", Target: filepath.Join(m.cfg.RootDir, "skills"), ReadOnly: true},
		{Name: "data", Target: filepath.Join(m.cfg.RootDir, "data"), ReadOnly: false},
	}
	bwrapCfg := BwrapConfig{
		ShareNet: m.cfg.NetworkMode != "" && m.cfg.NetworkMode != "none",
	}
	sb, err := NewLocalSandbox(sessionID, rootDir,
		WithIsolation(m.localIsolation),
		WithSymlinks(specs),
		WithBwrapConfig(bwrapCfg),
	)
	if err != nil {
		return nil, "", err
	}
	return sb, rootDir, nil
}

func (m *Manager) destroyLocked(sessionID string) error {
	e, ok := m.entries[sessionID]
	if !ok {
		return nil
	}
	delete(m.entries, sessionID)

	if err := e.sb.Close(); err != nil {
		telemetry.Warn(m.ctx, "sandbox close error",
			otellog.String("session", sessionID),
			otellog.String("error", err.Error()))
	}

	m.cleanupOverlay(sessionID)

	if e.rootDir != "" {
		if err := os.RemoveAll(e.rootDir); err != nil {
			telemetry.Warn(m.ctx, "sandbox cleanup error",
				otellog.String("session", sessionID),
				otellog.String("error", err.Error()))
		}
	}
	sbContainersActive.Add(m.ctx, -1)
	sbContainersDestroyed.Add(m.ctx, 1)
	telemetry.Info(m.ctx, "sandbox destroyed",
		otellog.String("session", sessionID))
	return nil
}

func (m *Manager) reaper() {
	defer m.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case now := <-ticker.C:
			m.reapIdle(now)
		}
	}
}

// resolveMounts processes the configured mounts for a session. Overlay-flagged
// mounts are prepared via OverlayManager (source replaced with the merged dir)
// when available; otherwise they fall back to a direct read-write bind mount.
func (m *Manager) resolveMounts(ctx context.Context, sessionID string) []MountConfig {
	resolved := make([]MountConfig, 0, len(m.cfg.Mounts))
	for _, mc := range m.cfg.Mounts {
		if !mc.Overlay || m.overlay == nil {
			resolved = append(resolved, mc)
			continue
		}
		od, err := m.overlay.Prepare(sessionID, mc.Source, mc.Target)
		if err != nil {
			telemetry.Warn(ctx, "overlay prepare failed, falling back to direct mount",
				otellog.String("session", sessionID),
				otellog.String("target", mc.Target),
				otellog.String("error", err.Error()))
			fallback := mc
			fallback.Overlay = false
			fallback.ReadOnly = false
			resolved = append(resolved, fallback)
			continue
		}
		resolved = append(resolved, MountConfig{
			Source: od.Merged,
			Target: mc.Target,
		})
	}
	return resolved
}

func (m *Manager) cleanupOverlay(sessionID string) {
	if m.overlay == nil {
		return
	}
	if err := m.overlay.Cleanup(sessionID); err != nil {
		telemetry.Warn(m.ctx, "overlay cleanup error",
			otellog.String("session", sessionID),
			otellog.String("error", err.Error()))
	}
}

func hasOverlayMounts(mounts []MountConfig) bool {
	for _, mc := range mounts {
		if mc.Overlay {
			return true
		}
	}
	return false
}

func (m *Manager) reapIdle(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for sid, e := range m.entries {
		if e.mode != ModeSession || e.refCount > 0 {
			continue
		}
		if now.Sub(e.lastUsed) > m.cfg.IdleTimeout {
			telemetry.Info(m.ctx, "sandbox reaping idle",
				otellog.String("session", sid))
			_ = m.destroyLocked(sid)
		}
	}
}
