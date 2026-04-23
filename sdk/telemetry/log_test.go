package telemetry

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/log/logtest"
)

// makeRecord builds a Record without the default length/count limits that
// logtest.RecordFactory's zero value would otherwise apply (which truncate
// string attribute values to ""). Use this everywhere instead of
// `var rec sdklog.Record` so attribute values survive round-tripping.
func makeRecord(sev otellog.Severity, body string, attrs ...otellog.KeyValue) sdklog.Record {
	return logtest.RecordFactory{
		Timestamp:                 time.Now(),
		Severity:                  sev,
		Body:                      otellog.StringValue(body),
		Attributes:                attrs,
		AttributeValueLengthLimit: -1,
		AttributeCountLimit:       -1,
	}.NewRecord()
}

func TestInitLog_NoExporter_DiscardStillEnables(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitLog(ctx)
	if err != nil {
		t.Fatalf("InitLog error: %v", err)
	}

	l := Logger("flowcraft/test")
	if l == nil {
		t.Fatalf("expected non-nil logger")
	}
	if !l.Enabled(ctx, otellog.EnabledParameters{Severity: otellog.SeverityInfo}) {
		t.Fatalf("expected logger to be enabled at info")
	}

	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Len()
}

func TestPlainTextExporter_Export_WritesEachRecord(t *testing.T) {
	var out lockedBuffer
	e := NewPlainTextExporter(&out)
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })

	recs := []sdklog.Record{
		makeRecord(otellog.SeverityInfo, "first"),
		makeRecord(otellog.SeverityInfo, "second"),
	}
	if err := e.Export(context.Background(), recs); err != nil {
		t.Fatalf("Export error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("expected both records in output, got %q", got)
	}
	if strings.Count(got, "\n") != 2 {
		t.Fatalf("expected exactly 2 lines, got %q", got)
	}
}

func TestPlainTextExporter_Export_WrappedInBatchProcessor(t *testing.T) {
	// End-to-end: exporter wrapped in BatchProcessor — what ConsoleProcessors
	// actually produces. Verifies that records flushed via the standard
	// ForceFlush/Shutdown path reach the writer.
	var out lockedBuffer
	bp := sdklog.NewBatchProcessor(NewPlainTextExporter(&out))
	t.Cleanup(func() { _ = bp.Shutdown(context.Background()) })

	rec := makeRecord(otellog.SeverityInfo, "via-batch")
	if err := bp.OnEmit(context.Background(), &rec); err != nil {
		t.Fatalf("OnEmit error: %v", err)
	}
	if err := bp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush error: %v", err)
	}
	if !strings.Contains(out.String(), "via-batch") {
		t.Fatalf("expected message in output, got %q", out.String())
	}
}

func TestConvenienceLogFunctions(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitLog(ctx)
	if err != nil {
		t.Fatalf("InitLog error: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	Enable()
	Info(ctx, "test info message")

	Disable()
	Info(ctx, "should be dropped")

	Enable()
	Info(ctx, "back again")
}

func TestWithLogProcessor(t *testing.T) {
	o := &logOptions{}
	WithLogProcessor(nil)(o)
	if len(o.processors) != 0 {
		t.Fatal("nil processor must be ignored")
	}

	p1 := discardProcessor{minSeverity: otellog.SeverityInfo}
	p2 := discardProcessor{minSeverity: otellog.SeverityWarn}
	WithLogProcessor(p1)(o)
	WithLogProcessor(p2)(o)
	if len(o.processors) != 2 {
		t.Fatalf("expected 2 processors after two calls, got %d", len(o.processors))
	}
}

func TestWithLogServiceName(t *testing.T) {
	o := &logOptions{}
	WithLogServiceName("svc")(o)
	if o.serviceName != "svc" {
		t.Fatalf("expected 'svc', got %q", o.serviceName)
	}
}

func TestWithLogServiceVersion(t *testing.T) {
	o := &logOptions{}
	WithLogServiceVersion("2.0")(o)
	if o.serviceVersion != "2.0" {
		t.Fatalf("expected '2.0', got %q", o.serviceVersion)
	}
}

func TestInitLog_NilContext(t *testing.T) {
	shutdown, err := InitLog(nil)
	if err != nil {
		t.Fatalf("InitLog error: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

// recordingProcessor captures records for inspection in tests.
type recordingProcessor struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (p *recordingProcessor) Enabled(_ context.Context, _ sdklog.EnabledParameters) bool {
	return true
}

func (p *recordingProcessor) OnEmit(_ context.Context, r *sdklog.Record) error {
	if r == nil {
		return nil
	}
	p.mu.Lock()
	p.records = append(p.records, *r)
	p.mu.Unlock()
	return nil
}

func (p *recordingProcessor) Shutdown(context.Context) error   { return nil }
func (p *recordingProcessor) ForceFlush(context.Context) error { return nil }

func (p *recordingProcessor) snapshot() []sdklog.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]sdklog.Record, len(p.records))
	copy(out, p.records)
	return out
}

func TestSeverityFilter_NilBase(t *testing.T) {
	p := NewSeverityFilter(nil, otellog.SeverityInfo, 0)
	_, ok := p.(discardProcessor)
	if !ok {
		t.Fatal("expected discardProcessor when base is nil")
	}
}

func TestSeverityFilter_LowerBoundOnly(t *testing.T) {
	base := &recordingProcessor{}
	p := NewSeverityFilter(base, otellog.SeverityWarn, 0).(*severityFilterProcessor)

	if p.Enabled(context.Background(), sdklog.EnabledParameters{Severity: otellog.SeverityInfo}) {
		t.Fatal("expected info to be filtered below warn")
	}
	if !p.Enabled(context.Background(), sdklog.EnabledParameters{Severity: otellog.SeverityWarn}) {
		t.Fatal("expected warn to be enabled")
	}
	if !p.Enabled(context.Background(), sdklog.EnabledParameters{Severity: otellog.SeverityError}) {
		t.Fatal("expected error to be enabled")
	}
}

func TestSeverityFilter_Range(t *testing.T) {
	base := &recordingProcessor{}
	// Allow [Info, Warn): info passes, warn dropped.
	p := NewSeverityFilter(base, otellog.SeverityInfo, otellog.SeverityWarn).(*severityFilterProcessor)

	for _, tc := range []struct {
		sev   otellog.Severity
		allow bool
	}{
		{otellog.SeverityDebug, false},
		{otellog.SeverityInfo, true},
		{otellog.SeverityWarn, false},
		{otellog.SeverityError, false},
	} {
		got := p.Enabled(context.Background(), sdklog.EnabledParameters{Severity: tc.sev})
		if got != tc.allow {
			t.Fatalf("severity %v: want allow=%v got %v", tc.sev, tc.allow, got)
		}
	}
}

func TestSeverityFilter_OnEmit(t *testing.T) {
	base := &recordingProcessor{}
	p := NewSeverityFilter(base, otellog.SeverityWarn, 0).(*severityFilterProcessor)

	infoRec := makeRecord(otellog.SeverityInfo, "below threshold")
	if err := p.OnEmit(context.Background(), &infoRec); err != nil {
		t.Fatalf("OnEmit error: %v", err)
	}

	if err := p.OnEmit(context.Background(), nil); err != nil {
		t.Fatalf("OnEmit nil record error: %v", err)
	}

	warnRec := makeRecord(otellog.SeverityWarn, "above threshold")
	if err := p.OnEmit(context.Background(), &warnRec); err != nil {
		t.Fatalf("OnEmit error: %v", err)
	}

	got := base.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 record to pass the filter, got %d", len(got))
	}
	if got[0].Severity() != otellog.SeverityWarn {
		t.Fatalf("expected the warn record to pass, got %v", got[0].Severity())
	}
}

func TestSeverityFilter_ShutdownAndFlush(t *testing.T) {
	base := &recordingProcessor{}
	p := NewSeverityFilter(base, otellog.SeverityInfo, 0).(*severityFilterProcessor)

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown error: %v", err)
	}
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush error: %v", err)
	}
}

func TestDiscardProcessor_ForceFlush(t *testing.T) {
	p := discardProcessor{minSeverity: otellog.SeverityInfo}
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush error: %v", err)
	}
}

func TestPlainTextExporter_Export_EmptyBatch(t *testing.T) {
	var out lockedBuffer
	e := NewPlainTextExporter(&out)
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })

	if err := e.Export(context.Background(), nil); err != nil {
		t.Fatalf("Export(nil) error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("expected no output, got %q", out.String())
	}
}

func TestPlainTextExporter_Export_CancelledContext(t *testing.T) {
	var out lockedBuffer
	e := NewPlainTextExporter(&out)
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := makeRecord(otellog.SeverityInfo, "x")
	if err := e.Export(ctx, []sdklog.Record{rec}); err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if out.Len() != 0 {
		t.Fatalf("expected no output on cancelled context, got %q", out.String())
	}
}

func TestPlainTextExporter_AfterShutdown_IsNoop(t *testing.T) {
	var out lockedBuffer
	e := NewPlainTextExporter(&out)

	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown error: %v", err)
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown error: %v", err)
	}

	rec := makeRecord(otellog.SeverityInfo, "after-shutdown")
	if err := e.Export(context.Background(), []sdklog.Record{rec}); err != nil {
		t.Fatalf("Export after Shutdown should be no-op, got: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("expected no output after Shutdown, got %q", out.String())
	}

	if err := e.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush after Shutdown error: %v", err)
	}
}

func TestNewPlainTextExporter_NilWriter_FallsBackToDiscard(t *testing.T) {
	e := NewPlainTextExporter(nil)
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })

	rec := makeRecord(otellog.SeverityInfo, "discarded")
	if err := e.Export(context.Background(), []sdklog.Record{rec}); err != nil {
		t.Fatalf("Export to nil writer should not error, got: %v", err)
	}
}

// failingWriter returns an error on every Write — used to verify that
// plainTextExporter.Export propagates I/O errors back to the caller
// (which the wrapping BatchProcessor will hand off to otel.Handle).
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errFailingWriter }

var errFailingWriter = errSentinel("failing writer")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

func TestPlainTextExporter_Export_PropagatesWriteError(t *testing.T) {
	e := NewPlainTextExporter(failingWriter{})
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })

	rec := makeRecord(otellog.SeverityInfo, "boom")
	err := e.Export(context.Background(), []sdklog.Record{rec})
	if err == nil {
		t.Fatal("expected error from failing writer")
	}
	if !strings.Contains(err.Error(), "failing writer") {
		t.Fatalf("expected wrapped writer error, got: %v", err)
	}
}

// TestInitLog_ForwardCompatible_ConsoleViaProcessor exercises the
// canonical wiring (WithLogProcessor(ConsoleProcessors(...)...)) and
// ensures the recommended path actually initializes a usable LoggerProvider.
func TestInitLog_ForwardCompatible_ConsoleViaProcessor(t *testing.T) {
	ctx := context.Background()
	procs := ConsoleProcessors(otellog.SeverityInfo)
	shutdown, err := InitLog(ctx,
		WithLogProcessor(procs[0]),
		WithLogProcessor(procs[1]),
	)
	if err != nil {
		t.Fatalf("InitLog error: %v", err)
	}

	l := Logger("flowcraft/test")
	if !l.Enabled(ctx, otellog.EnabledParameters{Severity: otellog.SeverityInfo}) {
		t.Fatalf("expected logger to be enabled at info via WithLogProcessor")
	}

	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestConsoleProcessors_StdoutStderrSplit(t *testing.T) {
	// Verify the console split routes records to the right destination by
	// driving the two processors directly with crafted records.
	procs := ConsoleProcessors(otellog.SeverityInfo)
	if len(procs) != 2 {
		t.Fatalf("expected 2 console processors, got %d", len(procs))
	}

	// stdout processor — only records in [Info, Warn) should pass through.
	infoOK := procs[0].Enabled(context.Background(), sdklog.EnabledParameters{Severity: otellog.SeverityInfo})
	warnDropped := !procs[0].Enabled(context.Background(), sdklog.EnabledParameters{Severity: otellog.SeverityWarn})
	if !infoOK || !warnDropped {
		t.Fatalf("stdout processor: infoOK=%v warnDropped=%v", infoOK, warnDropped)
	}

	// stderr processor — only Warn+ should pass through.
	warnOK := procs[1].Enabled(context.Background(), sdklog.EnabledParameters{Severity: otellog.SeverityWarn})
	infoDropped := !procs[1].Enabled(context.Background(), sdklog.EnabledParameters{Severity: otellog.SeverityInfo})
	if !warnOK || !infoDropped {
		t.Fatalf("stderr processor: warnOK=%v infoDropped=%v", warnOK, infoDropped)
	}
}

func TestFormatPlainTextValue(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", `""`},
		{"simple", "simple"},
		{"has space", `"has space"`},
		{"has\ttab", `"has` + "\t" + `tab"`},
		{`has"quote`, `"has\"quote"`},
		{"has=eq", `"has=eq"`},
	}
	for _, tt := range tests {
		got := formatPlainTextValue(tt.input)
		if got != tt.want {
			t.Errorf("formatPlainTextValue(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatPlainTextRecordLine_WithAttributes(t *testing.T) {
	rec := makeRecord(
		otellog.SeverityInfo, "test message",
		otellog.String("key", "value"),
		otellog.Int64("count", 42),
		otellog.Bool("ok", true),
	)

	line := FormatPlainTextRecordLine(&rec)
	s := string(line)
	if !strings.Contains(s, "test message") {
		t.Fatalf("expected 'test message' in output, got %q", s)
	}
	if !strings.Contains(s, "key=value") {
		t.Fatalf("expected 'key=value' in output, got %q", s)
	}
	if !strings.Contains(s, "count=42") {
		t.Fatalf("expected 'count=42' in output, got %q", s)
	}
	if !strings.Contains(s, "ok=true") {
		t.Fatalf("expected 'ok=true' in output, got %q", s)
	}
}

func TestFormatPlainTextRecordLine_ZeroTimestamp(t *testing.T) {
	rec := logtest.RecordFactory{
		Severity:                  otellog.SeverityDebug,
		Body:                      otellog.StringValue("no timestamps"),
		AttributeValueLengthLimit: -1,
		AttributeCountLimit:       -1,
	}.NewRecord()

	line := FormatPlainTextRecordLine(&rec)
	if len(line) == 0 {
		t.Fatal("expected non-empty output")
	}
}
