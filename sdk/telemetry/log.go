package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type logOptions struct {
	export         sdklog.Exporter
	serviceName    string
	serviceVersion string
	console        bool
	minSeverity    otellog.Severity
}

// LogOption configures InitLog behaviour.
type LogOption func(*logOptions)

func WithLogExporter(exp sdklog.Exporter) LogOption {
	return func(o *logOptions) { o.export = exp }
}

func WithLogServiceName(name string) LogOption {
	return func(o *logOptions) { o.serviceName = name }
}

func WithLogServiceVersion(version string) LogOption {
	return func(o *logOptions) { o.serviceVersion = version }
}

func WithLogConsole(enabled bool) LogOption {
	return func(o *logOptions) { o.console = enabled }
}

func WithLogMinSeverity(sev otellog.Severity) LogOption {
	return func(o *logOptions) { o.minSeverity = sev }
}

// InitLog initializes the OpenTelemetry LoggerProvider.
//
// - WithLogExporter: logs are exported via BatchProcessor + severityFilterProcessor.
// - WithLogConsole (default true): logs are written to stdout/stderr via plainTextProcessor.
// - Neither: discardProcessor (noop).
//
// Multiple processors can be stacked (export + console simultaneously).
func InitLog(ctx context.Context, opts ...LogOption) (func(context.Context) error, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	o := &logOptions{
		serviceName:    ServiceName,
		serviceVersion: ServiceVersion,
		console:        true,
		minSeverity:    otellog.SeverityInfo,
	}
	for _, fn := range opts {
		fn(o)
	}

	res, err := buildResource(ctx, o.serviceName, o.serviceVersion)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create log resource: %w", err)
	}

	var processors []sdklog.Processor
	if o.export != nil {
		processors = append(processors,
			newSeverityFilterProcessor(sdklog.NewBatchProcessor(o.export), o.minSeverity))
	}
	if o.console {
		processors = append(processors,
			newPlainTextProcessor(os.Stdout, os.Stderr, o.minSeverity))
	}
	if len(processors) == 0 {
		processors = append(processors, discardProcessor{minSeverity: o.minSeverity})
	}

	lpOpts := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}
	for _, p := range processors {
		lpOpts = append(lpOpts, sdklog.WithProcessor(p))
	}
	lp := sdklog.NewLoggerProvider(lpOpts...)
	logglobal.SetLoggerProvider(lp)

	return lp.Shutdown, nil
}

// ---------------------------------------------------------------------------
// discardProcessor — noop log processor
// ---------------------------------------------------------------------------

type discardProcessor struct {
	minSeverity otellog.Severity
}

func (p discardProcessor) Enabled(_ context.Context, param sdklog.EnabledParameters) bool {
	return param.Severity >= p.minSeverity
}

func (discardProcessor) OnEmit(context.Context, *sdklog.Record) error { return nil }
func (discardProcessor) Shutdown(context.Context) error               { return nil }
func (discardProcessor) ForceFlush(context.Context) error             { return nil }

// ---------------------------------------------------------------------------
// severityFilterProcessor — decorates another processor with a severity gate
// ---------------------------------------------------------------------------

type severityFilterProcessor struct {
	base        sdklog.Processor
	minSeverity otellog.Severity
}

func newSeverityFilterProcessor(base sdklog.Processor, min otellog.Severity) sdklog.Processor {
	if base == nil {
		return discardProcessor{minSeverity: min}
	}
	return &severityFilterProcessor{base: base, minSeverity: min}
}

func (p *severityFilterProcessor) Enabled(ctx context.Context, param sdklog.EnabledParameters) bool {
	if param.Severity < p.minSeverity {
		return false
	}
	return p.base.Enabled(ctx, param)
}

func (p *severityFilterProcessor) OnEmit(ctx context.Context, record *sdklog.Record) error {
	if record == nil || record.Severity() < p.minSeverity {
		return nil
	}
	return p.base.OnEmit(ctx, record)
}

func (p *severityFilterProcessor) Shutdown(ctx context.Context) error { return p.base.Shutdown(ctx) }
func (p *severityFilterProcessor) ForceFlush(ctx context.Context) error {
	return p.base.ForceFlush(ctx)
}

// ---------------------------------------------------------------------------
// plainTextProcessor — batched console log writer
// ---------------------------------------------------------------------------

// plainTextProcessor buffers log lines and flushes them in batches to reduce
// I/O overhead from high-frequency logging. WARN and above go to stderr,
// everything else to stdout.
type plainTextProcessor struct {
	stdout      io.Writer
	stderr      io.Writer
	minSeverity otellog.Severity

	batchMaxBytes int
	batchInterval time.Duration

	mu        sync.Mutex
	stdoutBuf []byte
	stderrBuf []byte

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func newPlainTextProcessor(stdout, stderr io.Writer, min otellog.Severity) sdklog.Processor {
	return newPlainTextProcessorWithBatch(stdout, stderr, min, 32*1024, 200*time.Millisecond)
}

func newPlainTextProcessorWithBatch(stdout, stderr io.Writer, min otellog.Severity, batchMaxBytes int, batchInterval time.Duration) sdklog.Processor {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if batchMaxBytes <= 0 {
		batchMaxBytes = 32 * 1024
	}
	if batchInterval < 0 {
		batchInterval = 0
	}

	p := &plainTextProcessor{
		stdout:        stdout,
		stderr:        stderr,
		minSeverity:   min,
		batchMaxBytes: batchMaxBytes,
		batchInterval: batchInterval,
		done:          make(chan struct{}),
	}

	if p.batchInterval > 0 {
		p.wg.Add(1)
		ticker := time.NewTicker(p.batchInterval)
		go func() {
			defer p.wg.Done()
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := p.ForceFlush(context.Background()); err != nil {
						fmt.Fprintln(os.Stderr, "telemetry: plaintext flush error:", err)
					}
				case <-p.done:
					return
				}
			}
		}()
	}

	return p
}

func (p *plainTextProcessor) Enabled(_ context.Context, param sdklog.EnabledParameters) bool {
	return param.Severity >= p.minSeverity
}

func (p *plainTextProcessor) OnEmit(_ context.Context, record *sdklog.Record) error {
	if record == nil || record.Severity() < p.minSeverity {
		return nil
	}

	line := formatPlainTextRecordLine(record)

	p.mu.Lock()
	defer p.mu.Unlock()

	if record.Severity() >= otellog.SeverityWarn {
		p.stderrBuf = append(p.stderrBuf, line...)
		if len(p.stderrBuf) >= p.batchMaxBytes {
			return flushBuffer(p.stderr, &p.stderrBuf)
		}
		return nil
	}

	p.stdoutBuf = append(p.stdoutBuf, line...)
	if len(p.stdoutBuf) >= p.batchMaxBytes {
		return flushBuffer(p.stdout, &p.stdoutBuf)
	}
	return nil
}

func (p *plainTextProcessor) Shutdown(ctx context.Context) error {
	p.stopOnce.Do(func() { close(p.done) })
	p.wg.Wait()
	return p.ForceFlush(ctx)
}

func (p *plainTextProcessor) ForceFlush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	var errs []error
	if err := flushBuffer(p.stdout, &p.stdoutBuf); err != nil {
		errs = append(errs, err)
	}
	if err := flushBuffer(p.stderr, &p.stderrBuf); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func flushBuffer(w io.Writer, buf *[]byte) error {
	if w == nil || buf == nil || len(*buf) == 0 {
		return nil
	}
	b := *buf
	*buf = (*buf)[:0]
	return writeFull(w, b)
}

func writeFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// formatPlainTextRecordLine formats a log record as:
//
//	RFC3339Nano SEVERITY message k=v ...
func formatPlainTextRecordLine(record *sdklog.Record) []byte {
	ts := record.Timestamp()
	if ts.IsZero() {
		ts = record.ObservedTimestamp()
	}
	if ts.IsZero() {
		ts = time.Now()
	}

	var b strings.Builder
	b.Grow(256)
	b.WriteString(ts.UTC().Format(time.RFC3339Nano))
	b.WriteByte(' ')
	b.WriteString(strings.ToUpper(record.Severity().String()))
	b.WriteByte(' ')

	body := record.Body()
	msg := body.String()
	if body.Kind() == otellog.KindString {
		msg = body.AsString()
	}
	b.WriteString(msg)

	record.WalkAttributes(func(kv otellog.KeyValue) bool {
		b.WriteByte(' ')
		b.WriteString(kv.Key)
		b.WriteByte('=')
		b.WriteString(formatPlainTextValue(kv.Value.String()))
		return true
	})
	b.WriteByte('\n')
	return []byte(b.String())
}

func formatPlainTextValue(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\r\n\"=") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}
