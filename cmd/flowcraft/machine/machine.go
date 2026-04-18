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

// ResetScope selects what to clean during a reset.
type ResetScope int

const (
	ResetMachine ResetScope = iota // VM / machine state only; preserves user data
	ResetData                      // user data only; preserves machine
	ResetAll                       // everything under ~/.flowcraft
)

// Machine manages the lifecycle of the minimal `flowcraft server` child (Linux).
type Machine interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status(ctx context.Context) (*Status, error)
	Logs(ctx context.Context, w io.Writer) error
	Reset(ctx context.Context, scope ResetScope) error
	OpenWeb(ctx context.Context) error
}
