package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent"
)

// BindingFunc creates a named binding for script execution.
// The returned name becomes the global variable name in the script scope.
type BindingFunc func(ctx context.Context) (name string, value any)

// LateBindingFunc creates a named binding after ordinary bindings are built.
// The Env argument exposes the environment assembled so far, including the
// bindings map that late bindings may capture.
type LateBindingFunc func(ctx context.Context, env *agent.ScriptEnv) (name string, value any)

// Builder accumulates bridge funcs into a [Provider]. Ordinary funcs bind
// first, in insertion order; late funcs run after them and observe the
// environment built so far.
//
// A builder carries no assembly rules of its own: [Assemble] owns them —
// identifier legality, duplicate rejection, typed-nil providers — so every
// path into a script environment validates the same way.
//
// Configure a builder before it is used and never mutate it afterwards:
// [Builder.Bind] and [Builder.BindLate] only read the accumulated funcs, so
// a built provider is safe to share across concurrent script executions.
type Builder struct {
	fns  []BindingFunc
	late []LateBindingFunc
}

// NewBuilder returns an empty builder.
func NewBuilder() *Builder { return &Builder{} }

// Add appends ordinary binding functions. They execute in insertion order.
func (b *Builder) Add(fns ...BindingFunc) *Builder {
	b.fns = append(b.fns, fns...)
	return b
}

// AddIf appends ordinary binding functions only when cond holds — the
// opt-in shape for capability bindings whose dep may be unwired (nil
// workspace, nil sandbox runner, …).
func (b *Builder) AddIf(cond bool, fns ...BindingFunc) *Builder {
	if cond {
		b.fns = append(b.fns, fns...)
	}
	return b
}

// AddLate appends late binding functions. They execute after ordinary bindings.
func (b *Builder) AddLate(fns ...LateBindingFunc) *Builder {
	b.late = append(b.late, fns...)
	return b
}

// Bind implements [Provider] by evaluating the ordinary funcs.
func (b *Builder) Bind(inv Invocation) ([]Binding, error) {
	if b == nil {
		return nil, nil
	}
	out := make([]Binding, 0, len(b.fns))
	for _, fn := range b.fns {
		name, value := fn(inv.Context)
		out = append(out, Binding{Name: name, Value: value})
	}
	return out, nil
}

// BindLate implements [LateProvider] by evaluating the late funcs against
// the environment built from the ordinary phase.
func (b *Builder) BindLate(inv Invocation, env *agent.ScriptEnv) ([]Binding, error) {
	if b == nil {
		return nil, nil
	}
	out := make([]Binding, 0, len(b.late))
	for _, fn := range b.late {
		name, value := fn(inv.Context, env)
		out = append(out, Binding{Name: name, Value: value})
	}
	return out, nil
}

// BuildEnv assembles an environment from ordinary binding funcs — the
// one-call shape of [Assemble] over [NewBuilder]. Late bindings use the
// builder directly.
func BuildEnv(
	ctx context.Context,
	config map[string]any,
	fns ...BindingFunc,
) (*agent.ScriptEnv, error) {
	return Assemble(Invocation{Context: ctx}, config, NewBuilder().Add(fns...))
}
