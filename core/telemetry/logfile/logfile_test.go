package logfile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/log/logtest"
)

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

func TestNewExporter_RequiresPath(t *testing.T) {
	if _, err := NewExporter(Config{}); err == nil {
		t.Fatal("expected error for empty Path")
	}
}

func TestNewExporter_AppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	exp, err := NewExporter(Config{Path: filepath.Join(dir, "x.log")})
	if err != nil {
		t.Fatalf("NewExporter error: %v", err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(context.Background()) })
	if exp.w == nil {
		t.Fatal("expected non-nil underlying writer")
	}
}

func TestExporter_Export_WritesFormattedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")

	exp, err := NewExporter(Config{Path: path, MaxSizeMB: 1, MaxBackups: 1, MaxAgeDays: 1})
	if err != nil {
		t.Fatalf("NewExporter error: %v", err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(context.Background()) })

	rec := makeRecord(otellog.SeverityInfo, "hello world", otellog.String("k", "v"))

	if err := exp.Export(context.Background(), []sdklog.Record{rec}); err != nil {
		t.Fatalf("Export error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "hello world") {
		t.Fatalf("expected message in file, got %q", got)
	}
	if !strings.Contains(got, "k=v") {
		t.Fatalf("expected attribute in file, got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("expected trailing newline, got %q", got)
	}
}

func TestExporter_Export_EmptyBatch(t *testing.T) {
	dir := t.TempDir()
	exp, err := NewExporter(Config{Path: filepath.Join(dir, "x.log")})
	if err != nil {
		t.Fatalf("NewExporter error: %v", err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(context.Background()) })

	if err := exp.Export(context.Background(), nil); err != nil {
		t.Fatalf("Export(nil) error: %v", err)
	}
}

func TestExporter_ShutdownTwiceAndExportAfter(t *testing.T) {
	dir := t.TempDir()
	exp, err := NewExporter(Config{Path: filepath.Join(dir, "x.log")})
	if err != nil {
		t.Fatalf("NewExporter error: %v", err)
	}

	if err := exp.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown error: %v", err)
	}
	if err := exp.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown error: %v", err)
	}

	rec := makeRecord(otellog.SeverityInfo, "after shutdown")
	if err := exp.Export(context.Background(), []sdklog.Record{rec}); err != nil {
		t.Fatalf("Export after Shutdown should be no-op, got: %v", err)
	}
}

func TestExporter_ForceFlush(t *testing.T) {
	dir := t.TempDir()
	exp, err := NewExporter(Config{Path: filepath.Join(dir, "x.log")})
	if err != nil {
		t.Fatalf("NewExporter error: %v", err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(context.Background()) })

	if err := exp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush error: %v", err)
	}
}
