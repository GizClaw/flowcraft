package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/script"
)

// BindingFunc creates a named binding for script execution.
type BindingFunc = script.BindingFunc

// LateBindingFunc creates a named binding after ordinary bindings are built.
type LateBindingFunc = script.LateBindingFunc

// EnvBuilder assembles per-execution script environments.
type EnvBuilder = script.EnvBuilder

// NewEnvBuilder creates an EnvBuilder using config as the script config.
func NewEnvBuilder(config map[string]any) *EnvBuilder {
	return script.NewEnvBuilder(config)
}

// BuildEnv creates a script.Env from binding funcs evaluated against ctx.
func BuildEnv(ctx context.Context, config map[string]any, fns ...BindingFunc) *script.Env {
	return script.BuildEnv(ctx, config, fns...)
}
