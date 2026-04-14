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
)

func TestInitLog_NoExporter_DiscardStillEnables(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitLog(ctx, WithLogConsole(false))
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

func TestPlainTextProcessor_Batch_ForceFlush(t *testing.T) {
	var out lockedBuffer
	var errOut lockedBuffer

	p := newPlainTextProcessorWithBatch(&out, &errOut, otellog.SeverityInfo, 1024*1024, 0)

	var rec sdklog.Record
	now := time.Now()
	rec.SetObservedTimestamp(now)
	rec.SetTimestamp(now)
	rec.SetSeverity(otellog.SeverityInfo)
	rec.SetBody(otellog.StringValue("hello"))

	if err := p.OnEmit(context.Background(), &rec); err != nil {
		t.Fatalf("OnEmit error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("expected buffered output before ForceFlush, got %q", out.String())
	}

	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush error: %v", err)
	}
	if out.Len() == 0 {
		t.Fatalf("expected output after ForceFlush")
	}
}

func TestPlainTextProcessor_Batch_TimeFlush(t *testing.T) {
	var out lockedBuffer
	var errOut lockedBuffer

	p := newPlainTextProcessorWithBatch(&out, &errOut, otellog.SeverityInfo, 1024*1024, 10*time.Millisecond)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	var rec sdklog.Record
	now := time.Now()
	rec.SetObservedTimestamp(now)
	rec.SetTimestamp(now)
	rec.SetSeverity(otellog.SeverityInfo)
	rec.SetBody(otellog.StringValue("tick"))

	if err := p.OnEmit(context.Background(), &rec); err != nil {
		t.Fatalf("OnEmit error: %v", err)
	}

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && out.Len() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if out.Len() == 0 {
		t.Fatalf("expected output after time-based flush")
	}
}

func TestConvenienceLogFunctions(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitLog(ctx, WithLogConsole(false))
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

func TestWithLogExporter(t *testing.T) {
	o := &logOptions{}
	WithLogExporter(nil)(o)
	if o.export != nil {
		t.Fatal("expected nil exporter")
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

func TestWithLogMinSeverity(t *testing.T) {
	o := &logOptions{}
	WithLogMinSeverity(otellog.SeverityWarn)(o)
	if o.minSeverity != otellog.SeverityWarn {
		t.Fatalf("expected SeverityWarn, got %v", o.minSeverity)
	}
}

func TestInitLog_NilContext(t *testing.T) {
	shutdown, err := InitLog(nil, WithLogConsole(false))
	if err != nil {
		t.Fatalf("InitLog error: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestSeverityFilterProcessor_NilBase(t *testing.T) {
	p := newSeverityFilterProcessor(nil, otellog.SeverityInfo)
	_, ok := p.(discardProcessor)
	if !ok {
		t.Fatal("expected discardProcessor when base is nil")
	}
}

func TestSeverityFilterProcessor_Enabled(t *testing.T) {
	base := discardProcessor{minSeverity: otellog.SeverityInfo}
	p := newSeverityFilterProcessor(base, otellog.SeverityWarn)
	sfp := p.(*severityFilterProcessor)

	if sfp.Enabled(context.Background(), sdklog.EnabledParameters{Severity: otellog.SeverityInfo}) {
		t.Fatal("expected info to be filtered below warn")
	}
	if !sfp.Enabled(context.Background(), sdklog.EnabledParameters{Severity: otellog.SeverityWarn}) {
		t.Fatal("expected warn to be enabled")
	}
	if !sfp.Enabled(context.Background(), sdklog.EnabledParameters{Severity: otellog.SeverityError}) {
		t.Fatal("expected error to be enabled")
	}
}

func TestSeverityFilterProcessor_OnEmit(t *testing.T) {
	base := discardProcessor{minSeverity: otellog.SeverityInfo}
	p := newSeverityFilterProcessor(base, otellog.SeverityWarn).(*severityFilterProcessor)

	var infoRec sdklog.Record
	infoRec.SetSeverity(otellog.SeverityInfo)
	infoRec.SetBody(otellog.StringValue("below threshold"))
	if err := p.OnEmit(context.Background(), &infoRec); err != nil {
		t.Fatalf("OnEmit error: %v", err)
	}

	if err := p.OnEmit(context.Background(), nil); err != nil {
		t.Fatalf("OnEmit nil record error: %v", err)
	}

	var warnRec sdklog.Record
	warnRec.SetSeverity(otellog.SeverityWarn)
	warnRec.SetBody(otellog.StringValue("above threshold"))
	if err := p.OnEmit(context.Background(), &warnRec); err != nil {
		t.Fatalf("OnEmit error: %v", err)
	}
}

func TestSeverityFilterProcessor_ShutdownAndFlush(t *testing.T) {
	base := discardProcessor{minSeverity: otellog.SeverityInfo}
	p := newSeverityFilterProcessor(base, otellog.SeverityInfo).(*severityFilterProcessor)

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

func TestPlainTextProcessor_Enabled(t *testing.T) {
	var out, errOut lockedBuffer
	p := newPlainTextProcessorWithBatch(&out, &errOut, otellog.SeverityWarn, 1024, 0)

	ptp := p.(*plainTextProcessor)
	if ptp.Enabled(context.Background(), sdklog.EnabledParameters{Severity: otellog.SeverityInfo}) {
		t.Fatal("expected info to be disabled with minSeverity=warn")
	}
	if !ptp.Enabled(context.Background(), sdklog.EnabledParameters{Severity: otellog.SeverityWarn}) {
		t.Fatal("expected warn to be enabled")
	}
}

func TestPlainTextProcessor_OnEmit_Nil(t *testing.T) {
	var out, errOut lockedBuffer
	p := newPlainTextProcessorWithBatch(&out, &errOut, otellog.SeverityInfo, 1024, 0)

	if err := p.OnEmit(context.Background(), nil); err != nil {
		t.Fatalf("OnEmit nil error: %v", err)
	}
}

func TestPlainTextProcessor_OnEmit_BelowMinSeverity(t *testing.T) {
	var out, errOut lockedBuffer
	p := newPlainTextProcessorWithBatch(&out, &errOut, otellog.SeverityWarn, 1024, 0)

	var rec sdklog.Record
	rec.SetSeverity(otellog.SeverityInfo)
	rec.SetBody(otellog.StringValue("too low"))

	if err := p.OnEmit(context.Background(), &rec); err != nil {
		t.Fatalf("OnEmit error: %v", err)
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Fatal("expected no output for record below min severity")
	}
}

func TestPlainTextProcessor_OnEmit_WarnGoesToStderr(t *testing.T) {
	var out, errOut lockedBuffer
	p := newPlainTextProcessorWithBatch(&out, &errOut, otellog.SeverityInfo, 1024*1024, 0)

	var rec sdklog.Record
	rec.SetSeverity(otellog.SeverityWarn)
	rec.SetTimestamp(time.Now())
	rec.SetBody(otellog.StringValue("warning msg"))

	if err := p.OnEmit(context.Background(), &rec); err != nil {
		t.Fatalf("OnEmit error: %v", err)
	}
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush error: %v", err)
	}
	if errOut.Len() == 0 {
		t.Fatal("expected stderr output for warn-level record")
	}
	if out.Len() != 0 {
		t.Fatal("expected no stdout output for warn-level record")
	}
}

func TestPlainTextProcessor_OnEmit_BatchOverflow(t *testing.T) {
	var out, errOut lockedBuffer
	p := newPlainTextProcessorWithBatch(&out, &errOut, otellog.SeverityInfo, 32, 0)

	var rec sdklog.Record
	rec.SetSeverity(otellog.SeverityInfo)
	rec.SetTimestamp(time.Now())
	rec.SetBody(otellog.StringValue("a long message that triggers immediate flush due to small batch size"))

	if err := p.OnEmit(context.Background(), &rec); err != nil {
		t.Fatalf("OnEmit error: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("expected immediate flush when buffer exceeds batchMaxBytes")
	}
}

func TestPlainTextProcessor_OnEmit_StderrBatchOverflow(t *testing.T) {
	var out, errOut lockedBuffer
	p := newPlainTextProcessorWithBatch(&out, &errOut, otellog.SeverityInfo, 32, 0)

	var rec sdklog.Record
	rec.SetSeverity(otellog.SeverityError)
	rec.SetTimestamp(time.Now())
	rec.SetBody(otellog.StringValue("a long error that triggers immediate flush due to small batch"))

	if err := p.OnEmit(context.Background(), &rec); err != nil {
		t.Fatalf("OnEmit error: %v", err)
	}
	if errOut.Len() == 0 {
		t.Fatal("expected immediate stderr flush when buffer exceeds batchMaxBytes")
	}
}

func TestPlainTextProcessor_ForceFlush_CancelledContext(t *testing.T) {
	var out, errOut lockedBuffer
	p := newPlainTextProcessorWithBatch(&out, &errOut, otellog.SeverityInfo, 1024, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.ForceFlush(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestPlainTextProcessor_ShutdownTwice(t *testing.T) {
	var out, errOut lockedBuffer
	p := newPlainTextProcessorWithBatch(&out, &errOut, otellog.SeverityInfo, 1024, 10*time.Millisecond)

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown error: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("second shutdown error: %v", err)
	}
}

func TestNewPlainTextProcessor_DefaultParams(t *testing.T) {
	p := newPlainTextProcessor(nil, nil, otellog.SeverityInfo)
	if p == nil {
		t.Fatal("expected non-nil processor")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestNewPlainTextProcessorWithBatch_NegativeInterval(t *testing.T) {
	p := newPlainTextProcessorWithBatch(nil, nil, otellog.SeverityInfo, -1, -1)
	if p == nil {
		t.Fatal("expected non-nil processor")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown error: %v", err)
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
	var rec sdklog.Record
	rec.SetSeverity(otellog.SeverityInfo)
	rec.SetTimestamp(time.Now())
	rec.SetBody(otellog.StringValue("test message"))
	rec.AddAttributes(otellog.String("key", "value"))

	line := formatPlainTextRecordLine(&rec)
	s := string(line)
	if !strings.Contains(s, "test message") {
		t.Fatalf("expected 'test message' in output, got %q", s)
	}
	if !strings.Contains(s, "key=") {
		t.Fatalf("expected 'key=' in output, got %q", s)
	}
}

func TestFormatPlainTextRecordLine_ZeroTimestamp(t *testing.T) {
	var rec sdklog.Record
	rec.SetSeverity(otellog.SeverityDebug)
	rec.SetBody(otellog.StringValue("no timestamps"))

	line := formatPlainTextRecordLine(&rec)
	if len(line) == 0 {
		t.Fatal("expected non-empty output")
	}
}
