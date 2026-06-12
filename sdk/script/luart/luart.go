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
	"time"

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

// WithMaxExecTime sets a runtime-enforced wall-clock ceiling on each
// Exec call. Independent of the caller's context: even ctx.Background
// cannot exceed d. The shorter of (caller deadline, d) wins. Zero
// disables the cap.
//
// On expiry the script is interrupted via gopher-lua's context hook
// and Exec returns a context-deadline error classified by
// sdk/errdefs.IsTimeout.
func WithMaxExecTime(d time.Duration) Option {
	return func(r *Runtime) {
		if d > 0 {
			r.maxExecTime = d
		}
	}
}

// Runtime manages a pool of gopher-lua VMs for Lua script execution.
// It implements script.Runtime. Close is safe to call multiple times.
//
// Note on memory caps: gopher-lua exposes no safe memory or
// instruction-count quota (its SetMx hook calls os.Exit on overflow,
// which is unusable in a library). The wall-clock ceiling installed
// via WithMaxExecTime is therefore the only "hard cut" available
// today; for stronger isolation run the script under a separate OS
// process with cgroup/rlimit applied.
type Runtime struct {
	pool        chan *lua.LState
	poolSize    int
	maxExecTime time.Duration
	once        sync.Once
	closeOnce   sync.Once
	closed      atomic.Bool
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

// SupportsNestedExec reports whether runtime.execScript has any chance of
// acquiring a second LState while a parent script is still holding one.
// Runtime bindings still use ExecNested so they can fail fast when busy.
func (r *Runtime) SupportsNestedExec() bool {
	return r != nil && r.poolSize > 1
}

func (r *Runtime) init() {
	r.once.Do(func() {
		r.pool = make(chan *lua.LState, r.poolSize)
		for i := 0; i < r.poolSize; i++ {
			r.pool <- r.newVM()
		}
	})
}

// newVM constructs one LState. Used by both pool init and the
// discard / replace path inside Exec so any future per-VM caps land
// in one place.
func (r *Runtime) newVM() *lua.LState {
	return lua.NewState()
}

// ErrVMPoolExhausted is returned when all VMs are in use and the context
// is cancelled before one becomes available.
var ErrVMPoolExhausted = errdefs.NotAvailable(errors.New("luart: VM pool exhausted, context cancelled while waiting"))

// ErrVMPoolBusy is returned by nested execution when no LState is immediately
// available. Nested scripts must not wait on the same pool their parent holds.
var ErrVMPoolBusy = errdefs.NotAvailable(errors.New("luart: VM pool exhausted"))

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

func (r *Runtime) tryAcquire() (*lua.LState, error) {
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
	default:
		return nil, ErrVMPoolBusy
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
	return r.exec(ctx, name, source, env, false)
}

// ExecNested runs a child script only when another LState is immediately
// available. It is used by runtime.execScript to avoid parent scripts
// deadlocking each other while they still hold their own LStates.
func (r *Runtime) ExecNested(ctx context.Context, name, source string, env *script.Env) (*script.Signal, error) {
	return r.exec(ctx, name, source, env, true)
}

func (r *Runtime) exec(ctx context.Context, name, source string, env *script.Env, nested bool) (*script.Signal, error) {
	if r.maxExecTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.maxExecTime)
		defer cancel()
	}

	var (
		L   *lua.LState
		err error
	)
	if nested {
		L, err = r.tryAcquire()
	} else {
		L, err = r.acquire(ctx)
	}
	if err != nil {
		return nil, err
	}

	L.RemoveContext()

	injectedNames := make([]string, 0, 8)
	defer func() {
		L.RemoveContext()
		for _, n := range injectedNames {
			L.SetGlobal(n, lua.LNil)
		}
		L.Close()
		r.release(r.newVM())
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
	// signal.interrupt and signal.error accept either:
	//   - a bare string (back-compat): used as Message; Kind stays empty
	//     (engine.CauseCustom / errdefs.Internal under SignalToError).
	//   - a table { kind, message, detail }: Kind classifies the signal
	//     per script.ErrorKind (errors) or engine.Cause (interrupts).
	//     Unknown Kind values degrade safely host-side rather than
	//     aborting the script.
	setGlobal("signal", pushGoValue(L, map[string]any{
		"interrupt": func(arg any) {
			kind, msg, detail := parseSignalArg(arg)
			sig = &script.Signal{Type: "interrupt", Kind: kind, Message: msg, Detail: detail}
			L.RaiseError(signalRaiseMarker)
		},
		"error": func(arg any) {
			kind, msg, detail := parseSignalArg(arg)
			sig = &script.Signal{Type: "error", Kind: kind, Message: msg, Detail: detail}
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
		if ctx.Err() != nil {
			return nil, errdefs.FromContext(fmt.Errorf("luart: script %q: execution cancelled: %w", name, ctx.Err()))
		}
		return nil, fmt.Errorf("luart: script %q: %w", name, runErr)
	}

	if sig != nil {
		return sig, nil
	}

	return nil, nil
}

// parseSignalArg decodes the polymorphic argument to signal.interrupt
// / signal.error. Strings populate Message only; tables may supply
// kind / message / detail keys. See jsrt.parseSignalArg for the JS
// twin — keeping the two implementations textually parallel is
// deliberate so a behavioural drift between runtimes is easy to spot.
func parseSignalArg(arg any) (kind, message string, detail map[string]any) {
	switch v := arg.(type) {
	case nil:
		return "", "", nil
	case string:
		return "", v, nil
	case map[string]any:
		if k, ok := v["kind"].(string); ok {
			kind = k
		}
		if m, ok := v["message"].(string); ok {
			message = m
		}
		if d, ok := v["detail"].(map[string]any); ok {
			detail = d
		}
		return kind, message, detail
	default:
		return "", fmt.Sprintf("%v", v), nil
	}
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
