//go:build !linux

package machine

import (
	"context"
	"errors"
	"io"
	"runtime"
)

// Native is a placeholder on unsupported platforms.
type Native struct{}

// NewNative returns a stub machine manager.
func NewNative() *Native {
	return &Native{}
}

var _ Machine = (*Native)(nil)

func (n *Native) Start(ctx context.Context) error {
	_ = ctx
	return errors.New("flowcraft start: unsupported platform " + runtime.GOOS)
}

func (n *Native) Stop(ctx context.Context) error {
	_ = ctx
	return errors.New("flowcraft stop: unsupported platform " + runtime.GOOS)
}

func (n *Native) Status(ctx context.Context) (*Status, error) {
	_ = ctx
	return &Status{}, nil
}

func (n *Native) Logs(ctx context.Context, w io.Writer) error {
	_ = ctx
	_ = w
	return errors.New("flowcraft logs: unsupported platform " + runtime.GOOS)
}

func (n *Native) Reset(ctx context.Context, scope ResetScope) error {
	_ = ctx
	_ = scope
	return errors.New("flowcraft reset: unsupported platform " + runtime.GOOS)
}

func (n *Native) OpenWeb(ctx context.Context) error {
	_ = ctx
	return errors.New("flowcraft web: unsupported platform " + runtime.GOOS)
}
