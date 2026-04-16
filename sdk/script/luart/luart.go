// Package luart provides a pure-Go Lua 5.1 implementation of script.Runtime
// using github.com/yuin/gopher-lua (no CGO).
package luart

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/script"
	lua "github.com/yuin/gopher-lua"
)

// Option configures a Runtime.
type Option func(*Runtime)

// WithPoolSize sets the VM pool size.
func WithPoolSize(n int) Option {
	return func(r *Runtime) {
		if n > 0 {
			r.poolSize = n
		}
	}
}

// Runtime manages a pool of gopher-lua VMs for Lua script execution.
// It implements script.Runtime. Close is safe to call multiple times.
type Runtime struct {
	pool      chan *lua.LState
	poolSize  int
	once      sync.Once
	closeOnce sync.Once
	closed    atomic.Bool
}

// New creates a new Lua runtime with a VM pool.
func New(opts ...Option) *Runtime {
	r := &Runtime{
		poolSize: runtime.NumCPU(),
	}
	if envVal := os.Getenv("FLOWCRAFT_LUA_POOL_SIZE"); envVal != "" {
		if n, err := strconv.Atoi(envVal); err == nil && n > 0 {
			r.poolSize = n
		}
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *Runtime) init() {
	r.once.Do(func() {
		r.pool = make(chan *lua.LState, r.poolSize)
		for i := 0; i < r.poolSize; i++ {
			r.pool <- lua.NewState()
		}
	})
}

// ErrVMPoolExhausted is returned when all VMs are in use and the context
// is cancelled before one becomes available.
var ErrVMPoolExhausted = errdefs.NotAvailable(errors.New("luart: VM pool exhausted, context cancelled while waiting"))

// ErrRuntimeClosed is returned when Exec is called after Close.
var ErrRuntimeClosed = errors.New("luart: runtime is closed")

func (r *Runtime) acquire(ctx context.Context) (*lua.LState, error) {
	if r.closed.Load() {
		return nil, ErrRuntimeClosed
	}
	r.init()
	select {
	case L := <-r.pool:
		if L == nil {
			return nil, ErrRuntimeClosed
		}
		return L, nil
	case <-ctx.Done():
		return nil, ErrVMPoolExhausted
	}
}

func (r *Runtime) release(L *lua.LState) {
	if r.closed.Load() {
		L.Close()
		return
	}
	r.pool <- L
}

// Close drains the VM pool and closes every LState. It is safe to call
// multiple times; subsequent calls are no-ops.
func (r *Runtime) Close() error {
	r.closed.Store(true)
	r.closeOnce.Do(func() {
		r.init()
		close(r.pool)
		for L := range r.pool {
			L.Close()
		}
	})
	return nil
}

// Exec implements script.Runtime. It runs Lua in a pooled LState with
// config and bindings as globals. A built-in "signal" global is always
// injected for interrupt/error/done control flow back to the host.
func (r *Runtime) Exec(ctx context.Context, name, source string, env *script.Env) (*script.Signal, error) {
	L, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}

	L.RemoveContext()

	injectedNames := make([]string, 0, 8)
	var discardVM bool
	defer func() {
		L.RemoveContext()
		for _, n := range injectedNames {
			L.SetGlobal(n, lua.LNil)
		}
		if discardVM {
			L.Close()
			r.release(lua.NewState())
		} else {
			r.release(L)
		}
	}()

	setGlobal := func(key string, val lua.LValue) {
		L.SetGlobal(key, val)
		injectedNames = append(injectedNames, key)
	}

	var config map[string]any
	if env != nil {
		config = env.Config
	}
	setGlobal("config", pushGoValue(L, config))

	if env != nil {
		for bname, bval := range env.Bindings {
			setGlobal(bname, pushGoValue(L, bval))
		}
	}

	var sig *script.Signal
	setGlobal("signal", pushGoValue(L, map[string]any{
		"interrupt": func(message string) {
			sig = &script.Signal{Type: "interrupt", Message: message}
			L.RaiseError(signalRaiseMarker)
		},
		"error": func(message string) {
			sig = &script.Signal{Type: "error", Message: message}
			L.RaiseError(signalRaiseMarker)
		},
		"done": func() {
			sig = &script.Signal{Type: "done"}
			L.RaiseError(signalRaiseMarker)
		},
	}))

	L.SetContext(ctx)
	runErr := L.DoString(wrapChunk(source))

	if runErr != nil {
		if sig != nil {
			return sig, nil
		}
		discardVM = true
		if ctx.Err() != nil {
			return nil, fmt.Errorf("luart: script %q: execution cancelled: %w", name, ctx.Err())
		}
		return nil, fmt.Errorf("luart: script %q: %w", name, runErr)
	}

	if sig != nil {
		return sig, nil
	}

	return nil, nil
}

func wrapChunk(source string) string {
	trimmed := strings.TrimSpace(source)
	if strings.HasPrefix(trimmed, "return (function") ||
		strings.HasPrefix(trimmed, "(function()") ||
		strings.HasPrefix(trimmed, "return function") {
		return source
	}
	return "return (function()\n" + source + "\nend)()\n"
}
