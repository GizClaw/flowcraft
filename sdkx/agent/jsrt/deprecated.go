// Package jsrt is deprecated. Use
// github.com/GizClaw/flowcraft/sdkx/agent/script/jsrt instead.
// This compatibility package will be removed in sdkx v0.6.0.
package jsrt

import (
	"time"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	newjsrt "github.com/GizClaw/flowcraft/sdkx/agent/script/jsrt"
)

type (
	Option           = newjsrt.Option
	Runtime          = newjsrt.Runtime
	ResourceSettings = newjsrt.ResourceSettings
)

const ResourceKind = newjsrt.ResourceKind

var (
	ErrVMPoolExhausted = newjsrt.ErrVMPoolExhausted
	ErrVMPoolBusy      = newjsrt.ErrVMPoolBusy
)

func New(opts ...newjsrt.Option) *newjsrt.Runtime {
	return newjsrt.New(opts...)
}

func NewDeployFactory() sdkconfig.Factory {
	return newjsrt.NewDeployFactory()
}

func WithPoolSize(n int) newjsrt.Option {
	return newjsrt.WithPoolSize(n)
}

func WithMaxCallStackSize(n int) newjsrt.Option {
	return newjsrt.WithMaxCallStackSize(n)
}

func WithMaxExecTime(d time.Duration) newjsrt.Option {
	return newjsrt.WithMaxExecTime(d)
}
