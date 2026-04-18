//go:build darwin

package machine

import (
	"context"
	"io"

	"github.com/GizClaw/flowcraft/cmd/flowcraft/machine/darwin"
)

// NewMachine returns a vfkit-based VM manager for macOS.
func NewMachine(version string) Machine {
	vm := darwin.NewVM(version, diskResolver, binaryResolver)
	return &darwinAdapter{vm: vm}
}

func diskResolver(ctx context.Context, version string) (string, error) {
	return EnsureImage(ctx, version, ImageDisk)
}

func binaryResolver(ctx context.Context, version string) (string, error) {
	return EnsureImage(ctx, version, ImageLinuxBin)
}

type darwinAdapter struct {
	vm *darwin.VM
}

func (a *darwinAdapter) Start(ctx context.Context) error {
	return a.vm.Start(ctx)
}

func (a *darwinAdapter) Stop(ctx context.Context) error {
	return a.vm.Stop(ctx)
}

func (a *darwinAdapter) Status(ctx context.Context) (*Status, error) {
	ds, err := a.vm.GetStatus(ctx)
	if err != nil {
		return nil, err
	}
	return &Status{
		Running:   ds.Running,
		PID:       ds.PID,
		HealthzOK: ds.HealthzOK,
	}, nil
}

func (a *darwinAdapter) Logs(ctx context.Context, w io.Writer) error {
	return a.vm.Logs(ctx, w)
}
