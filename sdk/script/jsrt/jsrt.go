// Package jsrt provides a goja-based JavaScript implementation of script.Runtime.
package jsrt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/dop251/goja"
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

// WithMaxCallStackSize bounds the maximum call-stack depth a script
// may reach. Hits raise a goja runtime error and abort the script.
// Zero / negative leaves goja's default in place.
func WithMaxCallStackSize(n int) Option {
	return func(r *Runtime) {
		if n > 0 {
			r.maxCallStackSize = n
		}
	}
}

// WithMaxExecTime sets a runtime-enforced wall-clock ceiling on each
// Exec call. The ceiling is independent of the caller's context: even
// if the caller passes context.Background(), no script may run longer
// than d. The shorter of (caller deadline, d) wins. Zero disables the
// extra cap (caller ctx alone applies).
//
// On expiry the script is interrupted via goja's Interrupt mechanism
// and Exec returns a context-deadline error classified by
// sdk/errdefs.IsTimeout.
func WithMaxExecTime(d time.Duration) Option {
	return func(r *Runtime) {
		if d > 0 {
			r.maxExecTime = d
		}
	}
}

// Runtime manages a pool of goja VMs for JS script execution.
// It implements script.Runtime.
type Runtime struct {
	pool             chan *goja.Runtime
	poolSize         int
	maxCallStackSize int
	maxExecTime      time.Duration
	once             sync.Once
}

// New creates a new JS runtime with a VM pool.
func New(opts ...Option) *Runtime {
	r := &Runtime{
		poolSize: runtime.NumCPU(),
	}
	if envVal := os.Getenv("FLOWCRAFT_JS_POOL_SIZE"); envVal != "" {
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
// acquiring a second VM while a parent script is still holding one. Runtime
// bindings still use ExecNested so they can fail fast when the pool is busy.
func (r *Runtime) SupportsNestedExec() bool {
	return r != nil && r.poolSize > 1
}

func (r *Runtime) init() {
	r.once.Do(func() {
		r.pool = make(chan *goja.Runtime, r.poolSize)
		for i := 0; i < r.poolSize; i++ {
			r.pool <- r.newVM()
		}
	})
}

// newVM constructs a single goja VM with the Runtime's per-VM caps
// applied. Centralised so pool init and any future replacement path
// stay in sync.
func (r *Runtime) newVM() *goja.Runtime {
	vm := goja.New()
	if r.maxCallStackSize > 0 {
		vm.SetMaxCallStackSize(r.maxCallStackSize)
	}
	return vm
}

// ErrVMPoolExhausted is returned when all VMs are in use and the context
// is cancelled before one becomes available.
var ErrVMPoolExhausted = errdefs.NotAvailable(errors.New("jsrt: VM pool exhausted, context cancelled while waiting"))

// ErrVMPoolBusy is returned by nested execution when no VM is immediately
// available. Nested scripts must not wait on the same pool their parent holds.
var ErrVMPoolBusy = errdefs.NotAvailable(errors.New("jsrt: VM pool exhausted"))

func (r *Runtime) acquire(ctx context.Context) (*goja.Runtime, error) {
	r.init()
	select {
	case vm := <-r.pool:
		return vm, nil
	case <-ctx.Done():
		return nil, ErrVMPoolExhausted
	}
}

func (r *Runtime) tryAcquire() (*goja.Runtime, error) {
	r.init()
	select {
	case vm := <-r.pool:
		return vm, nil
	default:
		return nil, ErrVMPoolBusy
	}
}

func (r *Runtime) release(vm *goja.Runtime) {
	r.pool <- vm
}

// Exec implements script.Runtime. It runs a JS script in a pooled VM
// with the given environment (config + bindings) injected as globals.
// A built-in "signal" global is always injected, providing interrupt/error/done
// control flow back to the host.
func (r *Runtime) Exec(ctx context.Context, name, source string, env *script.Env) (*script.Signal, error) {
	return r.exec(ctx, name, source, env, false)
}

// ExecNested runs a child script only when another VM is immediately
// available. It is used by runtime.execScript to avoid parent scripts
// deadlocking each other while they still hold their own VMs.
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
		vm  *goja.Runtime
		err error
	)
	if nested {
		vm, err = r.tryAcquire()
	} else {
		vm, err = r.acquire(ctx)
	}
	if err != nil {
		return nil, err
	}
	defer r.discardAndRelease(vm)

	vm.ClearInterrupt()

	var config map[string]any
	if env != nil {
		config = env.Config
	}
	if err := vm.Set("config", config); err != nil {
		return nil, fmt.Errorf("jsrt: set config: %w", err)
	}

	if env != nil {
		for bname, bval := range env.Bindings {
			if err := vm.Set(bname, bval); err != nil {
				return nil, fmt.Errorf("jsrt: set binding %q: %w", bname, err)
			}
		}
	}

	var sig *script.Signal
	// signal.interrupt and signal.error accept either:
	//   - a bare string (back-compat): used as Message; Kind stays empty
	//     (engine.CauseCustom / errdefs.Internal under SignalToError).
	//   - an object { kind, message, detail }: Kind classifies the
	//     signal per script.ErrorKind (errors) or engine.Cause
	//     (interrupts). Unknown Kind values degrade safely host-side
	//     rather than aborting the script.
	signalObj := map[string]any{
		"interrupt": func(arg any) {
			kind, msg, detail := parseSignalArg(arg)
			sig = &script.Signal{Type: "interrupt", Kind: kind, Message: msg, Detail: detail}
			vm.Interrupt("interrupt")
		},
		"error": func(arg any) {
			kind, msg, detail := parseSignalArg(arg)
			sig = &script.Signal{Type: "error", Kind: kind, Message: msg, Detail: detail}
			vm.Interrupt("error")
		},
		"done": func() {
			sig = &script.Signal{Type: "done"}
			vm.Interrupt("done")
		},
	}
	if err := vm.Set("signal", signalObj); err != nil {
		return nil, fmt.Errorf("jsrt: set signal: %w", err)
	}

	interruptDone := make(chan struct{})
	defer close(interruptDone)
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt("context cancelled: " + ctx.Err().Error())
		case <-interruptDone:
		}
	}()

	_, runErr := vm.RunString(wrapIIFE(source))

	if runErr != nil {
		if _, ok := runErr.(*goja.InterruptedError); ok {
			if sig != nil {
				return sig, nil
			}
			if ctx.Err() != nil {
				return nil, errdefs.FromContext(fmt.Errorf("jsrt: script %q: execution cancelled: %w", name, ctx.Err()))
			}
			return &script.Signal{Type: "interrupt"}, nil
		}
		return nil, enrichError(name, runErr)
	}

	if sig != nil {
		return sig, nil
	}

	return nil, nil
}

func (r *Runtime) discardAndRelease(_ *goja.Runtime) {
	r.release(r.newVM())
}

func wrapIIFE(source string) string {
	trimmed := strings.TrimSpace(source)
	if strings.HasPrefix(trimmed, "(function") || strings.HasPrefix(trimmed, "(()") {
		return source
	}
	return "(function(){\n" + source + "\n})();"
}

// parseSignalArg decodes the polymorphic argument to signal.interrupt
// / signal.error. Strings populate Message only; objects may supply
// kind / message / detail keys. Anything else is silently coerced to
// an empty message — invalid shapes do not abort the VM; the host's
// SignalToError translation handles unknown Kind values safely.
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
		// goja widens unrecognised JS values into interface{}; falling
		// through with a stringified form keeps the surface safe while
		// still leaving a trail for the script author to spot.
		return "", fmt.Sprintf("%v", v), nil
	}
}

func enrichError(scriptName string, err error) error {
	if ex, ok := err.(*goja.Exception); ok {
		return fmt.Errorf("jsrt: script %q: %s", scriptName, ex.String())
	}
	return fmt.Errorf("jsrt: script %q: %w", scriptName, err)
}
