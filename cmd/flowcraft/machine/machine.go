// Package machine abstracts how the FlowCraft HTTP server process is managed per OS.
package machine

import (
	"context"
	"io"
)

// Status describes a background server process.
type Status struct {
	Running   bool
	PID       int
	HealthzOK bool
}

// Machine manages the lifecycle of the minimal `flowcraft server` child (Linux).
type Machine interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status(ctx context.Context) (*Status, error)
	Logs(ctx context.Context, w io.Writer) error
}
