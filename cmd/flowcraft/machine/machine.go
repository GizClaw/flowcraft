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

// LogsSource selects which on-disk log file the Logs command reads.
type LogsSource int

const (
	// LogsServer reads the structured OTel log file written by the
	// running server (cfg.LogFilePath). Default.
	LogsServer LogsSource = iota
	// LogsCrash reads the stdout/stderr capture file used to surface
	// panics and any output emitted before telemetry initialized
	// (Linux: server.crash.log; not produced on macOS where the server
	// runs inside the guest).
	LogsCrash
	// LogsVM reads the vfkit console log (kernel + cloud-init + early
	// boot output). macOS-only; other platforms return an error.
	LogsVM
)

// LogsOptions controls Machine.Logs output. Zero value = full server log.
type LogsOptions struct {
	Source    LogsSource // which file to read; defaults to LogsServer
	TailLines int        // print only the last N lines; 0 = print all
}

// Machine manages the lifecycle of the minimal `flowcraft server` child (Linux).
type Machine interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status(ctx context.Context) (*Status, error)
	Logs(ctx context.Context, w io.Writer, opts LogsOptions) error
	Reset(ctx context.Context, scope ResetScope) error
	OpenWeb(ctx context.Context) error
}
