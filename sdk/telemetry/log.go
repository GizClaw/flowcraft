package telemetry

import (
	"context"
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
	export         sdklog.Exporter // deprecated: see WithLogExporter
	processors     []sdklog.Processor
	serviceName    string
	serviceVersion string
	console        bool
	minSeverity    otellog.Severity
}

// LogOption configures InitLog behaviour.
type LogOption func(*logOptions)

// WithLogExporter wires a single OTel log exporter wrapped in a default
// BatchProcessor with a min-severity gate.
//
// Deprecated: use WithLogProcessor for explicit control over batching and
// filtering. WithLogExporter has covering semantics (a second call replaces
// the first) and a hard-coded BatchProcessor configuration. It will be
// removed in v0.2.0.
//
// Migration:
//
//	// Before
//	telemetry.WithLogExporter(exp)
//
//	// After
//	telemetry.WithLogProcessor(
//	    telemetry.NewSeverityFilter(
//	        sdklog.NewBatchProcessor(exp),
//	        otellog.SeverityInfo, 0,
//	    ),
//	)
func WithLogExporter(exp sdklog.Exporter) LogOption {
	return func(o *logOptions) { o.export = exp }
}

// WithLogProcessor registers an OTel log processor. May be called multiple
// times to stack independent destinations (file, OTLP, custom routing).
//
// This mirrors OTel's own sdklog.NewLoggerProvider(WithProcessor(...))
// design and is the canonical way to attach log destinations going forward.
func WithLogProcessor(p sdklog.Processor) LogOption {
	return func(o *logOptions) {
		if p != nil {
			o.processors = append(o.processors, p)
		}
	}
}

func WithLogServiceName(name string) LogOption {
	return func(o *logOptions) { o.serviceName = name }
}

func WithLogServiceVersion(version string) LogOption {
	return func(o *logOptions) { o.serviceVersion = version }
}

// WithLogConsole toggles the default console sink.
//
// Deprecated: use WithLogProcessor with ConsoleProcessors for explicit
// control. WithLogConsole's "default true" semantics make it ambiguous
// whether console is intentionally on or just left at default; the
// explicit form makes intent visible at the call site. Will be removed
// in v0.2.0.
//
// Migration:
//
//	// Before — implicit default
//	telemetry.InitLog(ctx)
//
//	// After — explicit
//	telemetry.InitLog(ctx,
//	    telemetry.WithLogProcessor(telemetry.ConsoleProcessors(otellog.SeverityInfo)...),
//	)
//
//	// Before — disable console
//	telemetry.InitLog(ctx, telemetry.WithLogConsole(false))
//
//	// After — just don't pass ConsoleProcessors
//	telemetry.InitLog(ctx, telemetry.WithLogProcessor(myFileProc))
func WithLogConsole(enabled bool) LogOption {
	return func(o *logOptions) { o.console = enabled }
}

// WithLogMinSeverity sets the minimum severity for the deprecated
// WithLogExporter and WithLogConsole sinks.
//
// Deprecated: this option only affects the deprecated WithLogExporter
// and WithLogConsole sinks; it has no effect on processors registered
// via WithLogProcessor (those manage their own severity gate, typically
// by wrapping with NewSeverityFilter). With WithLogConsole and
// WithLogExporter slated for removal in v0.2.0, this option will go
// with them. Use NewSeverityFilter directly when wiring processors.
//
// Migration:
//
//	// Before
//	telemetry.InitLog(ctx, telemetry.WithLogMinSeverity(otellog.SeverityWarn))
//
//	// After
//	telemetry.InitLog(ctx,
//	    telemetry.WithLogProcessor(telemetry.ConsoleProcessors(otellog.SeverityWarn)...),
//	)
func WithLogMinSeverity(sev otellog.Severity) LogOption {
	return func(o *logOptions) { o.minSeverity = sev }
}

// InitLog initializes the OpenTelemetry LoggerProvider.
//
// Sinks are composed from (in order):
//  1. WithLogExporter (deprecated, single) — wrapped in BatchProcessor + severity gate.
//  2. WithLogProcessor (any number) — used as-is, callers control batching/filtering.
//  3. WithLogConsole (deprecated, default true) — stdout for INFO..<WARN,
//     stderr for WARN.. Equivalent to WithLogProcessor(ConsoleProcessors(...)...).
//
// If no sink is configured, a discardProcessor (noop) is installed.
//
// Forward-compatible usage (no deprecated options):
//
//	telemetry.InitLog(ctx,
//	    telemetry.WithLogProcessor(telemetry.ConsoleProcessors(otellog.SeverityInfo)...),
//	)
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
			NewSeverityFilter(sdklog.NewBatchProcessor(o.export), o.minSeverity, 0))
	}
	processors = append(processors, o.processors...)
	if o.console {
		processors = append(processors, ConsoleProcessors(o.minSeverity)...)
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

// ConsoleProcessors returns the canonical stdout/stderr split sink:
// records in [min, Warn) go to stdout, records in [Warn, +∞) go to
// stderr — mirroring POSIX conventions.
//
// Each side is a NewPlainTextExporter wrapped in sdklog.NewBatchProcessor
// for async batching and shutdown draining, then gated by NewSeverityFilter
// so OTel's Enabled protocol can short-circuit dropped records before
// formatting.
//
// Pass the result spread into WithLogProcessor:
//
//	telemetry.InitLog(ctx,
//	    telemetry.WithLogProcessor(telemetry.ConsoleProcessors(otellog.SeverityInfo)...),
//	)
//
// Each call returns a fresh slice of processors with their own batchers
// and exporters; do not share the returned processors across multiple
// LoggerProviders.
func ConsoleProcessors(min otellog.Severity) []sdklog.Processor {
	stdout := NewSeverityFilter(
		sdklog.NewBatchProcessor(NewPlainTextExporter(os.Stdout)),
		min, otellog.SeverityWarn,
	)
	stderr := NewSeverityFilter(
		sdklog.NewBatchProcessor(NewPlainTextExporter(os.Stderr)),
		otellog.SeverityWarn, 0,
	)
	return []sdklog.Processor{stdout, stderr}
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
	maxSeverity otellog.Severity // 0 = no upper bound
}

// NewSeverityFilter wraps base with a severity gate. Records with
// severity < min, or severity >= max when max != 0, are dropped before
// reaching base.
//
// Use max = 0 for "no upper bound" (the common case).
//
// The filter implements OTel's Enabled protocol, so dropped records are
// never constructed in the first place — saving CPU and allocations
// across the entire pipeline (formatting, batching, exporting).
//
// Returns a noop processor if base is nil.
func NewSeverityFilter(base sdklog.Processor, min, max otellog.Severity) sdklog.Processor {
	if base == nil {
		return discardProcessor{minSeverity: min}
	}
	return &severityFilterProcessor{base: base, minSeverity: min, maxSeverity: max}
}

func (p *severityFilterProcessor) allow(sev otellog.Severity) bool {
	if sev < p.minSeverity {
		return false
	}
	if p.maxSeverity != 0 && sev >= p.maxSeverity {
		return false
	}
	return true
}

func (p *severityFilterProcessor) Enabled(ctx context.Context, param sdklog.EnabledParameters) bool {
	if !p.allow(param.Severity) {
		return false
	}
	return p.base.Enabled(ctx, param)
}

func (p *severityFilterProcessor) OnEmit(ctx context.Context, record *sdklog.Record) error {
	if record == nil || !p.allow(record.Severity()) {
		return nil
	}
	return p.base.OnEmit(ctx, record)
}

func (p *severityFilterProcessor) Shutdown(ctx context.Context) error { return p.base.Shutdown(ctx) }
func (p *severityFilterProcessor) ForceFlush(ctx context.Context) error {
	return p.base.ForceFlush(ctx)
}

// ---------------------------------------------------------------------------
// plainTextExporter — formats and writes plain-text log lines to an io.Writer
// ---------------------------------------------------------------------------

// plainTextExporter implements sdklog.Exporter. Batching, queueing, retry
// policy, and shutdown draining are all delegated to a wrapping
// sdklog.BatchProcessor — this exporter only owns the
// "format record → write bytes" step.
//
// Concurrency: per the sdklog.Exporter contract, Export is never called
// concurrently with itself, so writes to w are naturally serialized
// without an explicit mutex. Shutdown may race with Export; the closed
// flag guards that boundary.
type plainTextExporter struct {
	w io.Writer

	mu     sync.Mutex
	closed bool
}

// NewPlainTextExporter returns an sdklog.Exporter that formats each
// record via FormatPlainTextRecordLine and writes it to w. All severities
// are written to the same w; for stdout/stderr splitting use
// ConsoleProcessors or compose two exporters with NewSeverityFilter.
//
// The exporter writes synchronously per Export call but is intended to
// be wrapped in sdklog.NewBatchProcessor (which provides asynchronous
// batching, queueing, and shutdown draining). ConsoleProcessors and the
// internal default sink already do this wrapping.
//
// A nil w is treated as io.Discard.
func NewPlainTextExporter(w io.Writer) sdklog.Exporter {
	if w == nil {
		w = io.Discard
	}
	return &plainTextExporter{w: w}
}

func (e *plainTextExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	for i := range records {
		line := FormatPlainTextRecordLine(&records[i])
		if err := writeFull(e.w, line); err != nil {
			return fmt.Errorf("telemetry: plaintext export: %w", err)
		}
	}
	return nil
}

func (e *plainTextExporter) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}

// ForceFlush is a no-op: writes are synchronous per Export call. Any
// caller-side batching lives in the wrapping BatchProcessor, whose own
// ForceFlush will drain pending records into Export before returning.
func (e *plainTextExporter) ForceFlush(_ context.Context) error { return nil }

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

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

// FormatPlainTextRecordLine renders an OTel log record as a single line
// in the canonical plain-text format used by NewPlainTextExporter:
//
//	RFC3339Nano SEVERITY message k=v ...
//
// Exposed so downstream sinks (file exporters, custom processors) can
// match the on-screen format.
func FormatPlainTextRecordLine(record *sdklog.Record) []byte {
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
		b.WriteString(formatPlainTextValue(stringifyLogValue(kv.Value)))
		return true
	})
	b.WriteByte('\n')
	return []byte(b.String())
}

// stringifyLogValue converts an otellog.Value to a string suitable for
// plain-text output. KindString is treated specially because Value.String
// only returns the raw string for that one Kind; for other Kinds it
// returns the type name, which is useless in logs.
func stringifyLogValue(v otellog.Value) string {
	switch v.Kind() {
	case otellog.KindString:
		return v.AsString()
	case otellog.KindBool:
		if v.AsBool() {
			return "true"
		}
		return "false"
	case otellog.KindInt64:
		return fmt.Sprintf("%d", v.AsInt64())
	case otellog.KindFloat64:
		return fmt.Sprintf("%g", v.AsFloat64())
	case otellog.KindBytes:
		return fmt.Sprintf("%x", v.AsBytes())
	default:
		return v.String()
	}
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
