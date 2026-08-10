// Package luart is deprecated. Use
// github.com/GizClaw/flowcraft/sdkx/agent/script/luart instead.
// This compatibility package will be removed in sdkx v0.6.0.
package luart

import (
	"time"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	newluart "github.com/GizClaw/flowcraft/sdkx/agent/script/luart"
)

type (
	Option           = newluart.Option
	Runtime          = newluart.Runtime
	ResourceSettings = newluart.ResourceSettings
)

const ResourceKind = newluart.ResourceKind

var (
	ErrVMPoolExhausted = newluart.ErrVMPoolExhausted
	ErrVMPoolBusy      = newluart.ErrVMPoolBusy
	ErrRuntimeClosed   = newluart.ErrRuntimeClosed
)

func New(opts ...newluart.Option) *newluart.Runtime {
	return newluart.New(opts...)
}

func NewDeployFactory() sdkconfig.Factory {
	return newluart.NewDeployFactory()
}

func WithPoolSize(n int) newluart.Option {
	return newluart.WithPoolSize(n)
}

func WithMaxExecTime(d time.Duration) newluart.Option {
	return newluart.WithMaxExecTime(d)
}
