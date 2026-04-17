package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// SandboxHandle manages a single runtime-scoped sandbox instance.
// The workspace directory is persistent; only the running process/container is recycled.
type SandboxHandle struct {
	ctx       context.Context
	runtimeID string
	cfg       ManagerConfig

	mu             sync.Mutex
	active         Sandbox
	activeRootDir  string
	useCount       int
	idleTimer      *time.Timer
	overlay        *OverlayManager
	localIsolation probeResult
	closed         bool
}

// NewSandboxHandle creates a runtime-scoped sandbox handle.
func NewSandboxHandle(ctx context.Context, runtimeID string, cfg ManagerConfig) (*SandboxHandle, error) {
	if runtimeID == "" {
		return nil, fmt.Errorf("sandbox: runtime ID is required")
	}

	cfg = normalizeHandleConfig(cfg)
	validateCfg := cfg
	if validateCfg.MaxConcurrent <= 0 {
		validateCfg.MaxConcurrent = 1
	}
	if err := validateCfg.Validate(); err != nil {
		return nil, err
	}

	var overlayMgr *OverlayManager
	if hasOverlayMounts(cfg.Mounts) && OverlaySupported() {
		overlayDir := filepath.Join(cfg.RootDir, ".overlays")
		om, err := NewOverlayManager(overlayDir)
		if err != nil {
			telemetry.Warn(ctx, "sandbox handle: overlay init failed, falling back to direct mount",
				otellog.String("error", err.Error()))
		} else {
			overlayMgr = om
		}
	}

	var localIso probeResult
	if cfg.Driver == "local" || cfg.Driver == "" {
		localIso = probeIsolation()
	}

	return &SandboxHandle{
		ctx:            ctx,
		runtimeID:      runtimeID,
		cfg:            cfg,
		overlay:        overlayMgr,
		localIsolation: localIso,
	}, nil
}

func normalizeHandleConfig(cfg ManagerConfig) ManagerConfig {
	if cfg.Driver == "" {
		cfg.Driver = "local"
	}
	if cfg.Mode == "" {
		cfg.Mode = ModePersistent
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = 5 * time.Minute
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	if cfg.RootDir == "" {
		cfg.RootDir = os.TempDir()
	}
	return cfg
}

// Acquire returns the active sandbox and a release callback.
func (h *SandboxHandle) Acquire(ctx context.Context) (Sandbox, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return nil, nil, ErrClosed
	}

	if h.active == nil {
		sb, rootDir, err := h.create(ctx)
		if err != nil {
			return nil, nil, err
		}
		h.active = sb
		h.activeRootDir = rootDir
	}

	h.useCount++
	h.stopIdleTimerLocked()

	var once sync.Once
	release := func() {
		once.Do(func() {
			h.Release()
		})
	}
	return h.active, release, nil
}

// Release decrements the active lease count.
func (h *SandboxHandle) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.useCount > 0 {
		h.useCount--
	}
	if h.useCount == 0 && h.active != nil && !h.closed {
		h.resetIdleTimerLocked()
	}
}

// Close terminates the active sandbox instance.
func (h *SandboxHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.closed = true
	h.stopIdleTimerLocked()
	return h.closeActiveLocked()
}

// Config returns the handle configuration.
func (h *SandboxHandle) Config() ManagerConfig {
	return h.cfg
}

// RuntimeID returns the bound runtime ID.
func (h *SandboxHandle) RuntimeID() string {
	return h.runtimeID
}

// UseCount reports the current lease count. Intended for tests/debugging.
func (h *SandboxHandle) UseCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.useCount
}

func (h *SandboxHandle) resetIdleTimerLocked() {
	if h.cfg.IdleTimeout <= 0 {
		return
	}
	if h.idleTimer == nil {
		h.idleTimer = time.AfterFunc(h.cfg.IdleTimeout, h.onIdle)
		return
	}
	h.idleTimer.Reset(h.cfg.IdleTimeout)
}

func (h *SandboxHandle) stopIdleTimerLocked() {
	if h.idleTimer != nil {
		h.idleTimer.Stop()
	}
}

func (h *SandboxHandle) onIdle() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed || h.useCount > 0 {
		return
	}
	if err := h.closeActiveLocked(); err != nil {
		telemetry.Warn(h.ctx, "sandbox handle: idle close failed",
			otellog.String("runtime_id", h.runtimeID),
			otellog.String("error", err.Error()))
	}
}

func (h *SandboxHandle) closeActiveLocked() error {
	if h.active == nil {
		return nil
	}
	err := h.active.Close()
	if err != nil {
		telemetry.Warn(h.ctx, "sandbox handle: close error",
			otellog.String("runtime_id", h.runtimeID),
			otellog.String("error", err.Error()))
	}
	if h.overlay != nil {
		if cleanErr := h.overlay.Cleanup(h.runtimeID); cleanErr != nil {
			telemetry.Warn(h.ctx, "sandbox handle: overlay cleanup error",
				otellog.String("runtime_id", h.runtimeID),
				otellog.String("error", cleanErr.Error()))
		}
	}
	h.active = nil
	h.activeRootDir = ""
	return err
}

func (h *SandboxHandle) create(ctx context.Context) (Sandbox, string, error) {
	switch h.cfg.Driver {
	case "local", "":
		rootDir := filepath.Join(h.cfg.RootDir, "local", h.runtimeID)
		specs := []SymlinkSpec{
			{Name: "skills", Target: filepath.Join(h.cfg.RootDir, "skills"), ReadOnly: true},
			{Name: "data", Target: filepath.Join(h.cfg.RootDir, "data"), ReadOnly: false},
		}
		bwrapCfg := BwrapConfig{
			ShareNet: h.cfg.NetworkMode != "" && h.cfg.NetworkMode != "none",
		}
		sb, err := NewLocalSandbox(h.runtimeID, rootDir,
			WithIsolation(h.localIsolation),
			WithSymlinks(specs),
			WithBwrapConfig(bwrapCfg),
		)
		if err != nil {
			return nil, "", err
		}
		return sb, rootDir, nil
	case "docker":
		mounts := h.resolveMounts(ctx, h.runtimeID)
		driver := NewDockerDriver(h.cfg.Image, mounts)
		createCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		sb, err := driver.Create(createCtx, h.runtimeID, CreateOptions{
			NetworkMode: h.cfg.NetworkMode,
			CPUQuota:    h.cfg.CPUQuota,
			MemoryLimit: h.cfg.MemoryLimit,
		})
		if err != nil {
			return nil, "", err
		}
		return sb, "", nil
	default:
		return nil, "", fmt.Errorf("sandbox: unsupported driver %q", h.cfg.Driver)
	}
}

func (h *SandboxHandle) resolveMounts(ctx context.Context, runtimeID string) []MountConfig {
	resolved := make([]MountConfig, 0, len(h.cfg.Mounts))
	for _, mc := range h.cfg.Mounts {
		if !mc.Overlay || h.overlay == nil {
			resolved = append(resolved, mc)
			continue
		}
		od, err := h.overlay.Prepare(runtimeID, mc.Source, mc.Target)
		if err != nil {
			telemetry.Warn(ctx, "sandbox handle: overlay prepare failed, falling back to direct mount",
				otellog.String("runtime_id", runtimeID),
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
