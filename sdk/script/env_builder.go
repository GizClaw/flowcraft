package script

import "context"

// BindingFunc creates a named binding for script execution.
// The returned name becomes the global variable name in the script scope.
type BindingFunc func(ctx context.Context) (name string, value any)

// LateBindingFunc creates a named binding after ordinary bindings are built.
// The Env argument exposes the final bindings map so late bindings can capture
// the same map passed to the runtime.
type LateBindingFunc func(ctx context.Context, env *Env) (name string, value any)

// EnvBuilder assembles per-execution script environments.
//
// Ordinary bindings run first in the order they were added. Late bindings run
// after ordinary bindings and receive the Env being built, including the final
// bindings map. If multiple bindings return the same name, the later binding
// wins. This preserves the map-based Env semantics while making order-sensitive
// bindings such as runtime.execScript explicit.
type EnvBuilder struct {
	config map[string]any
	fns    []BindingFunc
	late   []LateBindingFunc
}

// NewEnvBuilder creates an EnvBuilder using config as the script config.
func NewEnvBuilder(config map[string]any) *EnvBuilder {
	return &EnvBuilder{config: config}
}

// Add appends ordinary binding functions. They execute in insertion order.
func (b *EnvBuilder) Add(fns ...BindingFunc) *EnvBuilder {
	b.fns = append(b.fns, fns...)
	return b
}

// AddLate appends late binding functions. They execute after ordinary bindings.
func (b *EnvBuilder) AddLate(fns ...LateBindingFunc) *EnvBuilder {
	b.late = append(b.late, fns...)
	return b
}

// Build evaluates all bindings and returns a fresh Env.
func (b *EnvBuilder) Build(ctx context.Context) *Env {
	if b == nil {
		return &Env{Bindings: map[string]any{}}
	}
	env := &Env{
		Config:   b.config,
		Bindings: make(map[string]any, len(b.fns)+len(b.late)),
	}
	for _, fn := range b.fns {
		name, val := fn(ctx)
		env.Bindings[name] = val
	}
	for _, fn := range b.late {
		name, val := fn(ctx, env)
		env.Bindings[name] = val
	}
	return env
}

// BuildEnv creates a script Env from ordinary binding funcs.
func BuildEnv(ctx context.Context, config map[string]any, fns ...BindingFunc) *Env {
	return NewEnvBuilder(config).Add(fns...).Build(ctx)
}
