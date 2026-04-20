//go:build darwin

package machine

import (
	"context"
	"errors"
	"fmt"
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

func (a *darwinAdapter) Logs(_ context.Context, w io.Writer, opts LogsOptions) error {
	switch opts.Source {
	case LogsServer:
		path := a.vm.LogsServerPath()
		if path == "" {
			return errors.New("server log file is disabled (log.file.path is empty)")
		}
		return WriteLogFile(w, path, opts.TailLines, "no server log file yet")
	case LogsVM:
		return WriteLogFile(w, a.vm.VMLogPath(), opts.TailLines, "no VM log file yet")
	case LogsCrash:
		return errors.New("--crash log is not exposed from the macOS guest; use --vm for boot failures or omit to read the server log")
	default:
		return fmt.Errorf("unknown logs source %d", opts.Source)
	}
}

func (a *darwinAdapter) Reset(ctx context.Context, scope ResetScope) error {
	return a.vm.Reset(ctx, int(scope))
}

func (a *darwinAdapter) OpenWeb(ctx context.Context) error {
	return a.vm.OpenWeb(ctx)
}
