// Package logfile provides an OTel log Exporter that writes records to a
// rotating local file using natefinch/lumberjack.
//
// Records are formatted via telemetry.FormatPlainTextRecordLine so the
// on-disk format matches the console sink — the same line a developer
// would see on stderr is what ends up in the file.
//
// Typical wiring:
//
//	exp := logfile.NewExporter(logfile.Config{
//	    Path:       "/var/log/flowcraft/server.log",
//	    MaxSizeMB:  100,
//	    MaxBackups: 7,
//	    MaxAgeDays: 30,
//	})
//	telemetry.InitLog(ctx,
//	    telemetry.WithLogProcessor(
//	        sdklog.NewBatchProcessor(exp),
//	    ),
//	)
//
// Rotation is size-triggered only. Time-based rotation, log shipping, and
// other policies are out of scope — pair with logrotate or similar if
// needed.
package logfile

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Config controls the rotating file sink. All size fields are interpreted
// in megabytes / count / days, matching lumberjack's semantics.
type Config struct {
	// Path is the active log file. Required. Rotated files live alongside
	// it, named like "server-2026-04-20T10-15-30.123.log".
	Path string

	// MaxSizeMB is the size threshold (in MB) that triggers rotation.
	// Default: 100. Set to 0 to disable size-based rotation.
	MaxSizeMB int

	// MaxBackups is the maximum number of rotated files to retain.
	// Default: 7. Set to 0 to keep all rotated files.
	MaxBackups int

	// MaxAgeDays is the maximum age of rotated files in days.
	// Default: 30. Set to 0 to never delete based on age.
	MaxAgeDays int

	// Compress enables gzip compression for rotated files.
	// Default: false (CPU-cheap, easier ad-hoc inspection).
	Compress bool
}

const (
	defaultMaxSizeMB  = 100
	defaultMaxBackups = 7
	defaultMaxAgeDays = 30
)

// Exporter is an OTel sdklog.Exporter writing formatted records to a
// rotating file.
type Exporter struct {
	w  io.WriteCloser
	mu sync.Mutex
}

// NewExporter constructs an Exporter from cfg. Path is required; defaults
// are applied for any zero-valued size/retention field. The underlying
// file is opened lazily on the first write.
func NewExporter(cfg Config) (*Exporter, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("logfile: Config.Path is required")
	}
	if cfg.MaxSizeMB == 0 {
		cfg.MaxSizeMB = defaultMaxSizeMB
	}
	if cfg.MaxBackups == 0 {
		cfg.MaxBackups = defaultMaxBackups
	}
	if cfg.MaxAgeDays == 0 {
		cfg.MaxAgeDays = defaultMaxAgeDays
	}
	w := &lumberjack.Logger{
		Filename:   cfg.Path,
		MaxSize:    cfg.MaxSizeMB,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAgeDays,
		Compress:   cfg.Compress,
	}
	return &Exporter{w: w}, nil
}

// Export writes the supplied batch of records to the file.
func (e *Exporter) Export(_ context.Context, records []sdklog.Record) error {
	if len(records) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.w == nil {
		return nil
	}
	for i := range records {
		line := telemetry.FormatPlainTextRecordLine(&records[i])
		if _, err := e.w.Write(line); err != nil {
			return fmt.Errorf("logfile: write: %w", err)
		}
	}
	return nil
}

// Shutdown closes the underlying file. Subsequent Export calls are no-ops.
func (e *Exporter) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.w == nil {
		return nil
	}
	err := e.w.Close()
	e.w = nil
	return err
}

// ForceFlush is a no-op: lumberjack writes synchronously and any caller-
// side batching lives in the wrapping BatchProcessor.
func (e *Exporter) ForceFlush(_ context.Context) error { return nil }
