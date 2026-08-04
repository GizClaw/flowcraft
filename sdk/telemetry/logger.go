package telemetry

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

var (
	loggerName atomic.Value
	disabled   atomic.Bool
)

func init() {
	loggerName.Store("")
}

// SetLoggerName sets the scope name for the convenience log functions.
func SetLoggerName(name string) {
	loggerName.Store(strings.TrimSpace(name))
}

// Disable globally disables all convenience log functions (useful in tests).
func Disable() { disabled.Store(true) }

// Enable re-enables convenience log functions.
func Enable() { disabled.Store(false) }

// emit is the core logging function. It creates an OTel LogRecord, injects
// trace_id/span_id from ctx when available, and emits it through the global
// LoggerProvider.
func emit(ctx context.Context, severity otellog.Severity, msg string, attrs ...otellog.KeyValue) {
	if ctx == nil {
		ctx = context.Background()
	}
	if disabled.Load() {
		return
	}

	v := loggerName.Load()
	name, _ := v.(string)
	l := Logger(name)
	if !l.Enabled(ctx, otellog.EnabledParameters{Severity: severity}) {
		return
	}

	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs,
			otellog.String("trace_id", sc.TraceID().String()),
			otellog.String("span_id", sc.SpanID().String()),
		)
	}

	var rec otellog.Record
	now := time.Now()
	rec.SetTimestamp(now)
	rec.SetObservedTimestamp(now)
	rec.SetSeverity(severity)
	rec.SetBody(otellog.StringValue(msg))
	rec.AddAttributes(attrs...)

	l.Emit(ctx, rec)
}

func Trace(ctx context.Context, msg string, attrs ...otellog.KeyValue) {
	emit(ctx, otellog.SeverityTrace, msg, attrs...)
}

func Debug(ctx context.Context, msg string, attrs ...otellog.KeyValue) {
	emit(ctx, otellog.SeverityDebug, msg, attrs...)
}

func Info(ctx context.Context, msg string, attrs ...otellog.KeyValue) {
	emit(ctx, otellog.SeverityInfo, msg, attrs...)
}

func Warn(ctx context.Context, msg string, attrs ...otellog.KeyValue) {
	emit(ctx, otellog.SeverityWarn, msg, attrs...)
}

func Error(ctx context.Context, msg string, attrs ...otellog.KeyValue) {
	emit(ctx, otellog.SeverityError, msg, attrs...)
}

// WarnErr is a convenience for the very common SDK pattern of intentionally
// swallowing an error at a best-effort boundary (defer Close, deferred
// cleanup, post-error fallback) while still making the error visible to
// telemetry. The error, if non-nil, is attached as AttrErrorMessage so
// dashboards can filter swallowed errors uniformly across modules.
//
// Callers should still log through the regular Warn/Error helpers when the
// error is the primary outcome of a call — WarnErr exists specifically for
// the "we already returned a richer error / we already moved on, but the
// dropped value is still worth seeing" case.
//
// Example:
//
//	if err := f.Close(); err != nil {
//	    telemetry.WarnErr(ctx, "workspace: close after append", err,
//	        otellog.String("path", path))
//	}
//
// A nil err produces no log record; calling this with a nil error is a
// cheap no-op so defer chains can use it unconditionally.
func WarnErr(ctx context.Context, msg string, err error, attrs ...otellog.KeyValue) {
	if err == nil {
		return
	}
	attrs = append(attrs, otellog.String(AttrErrorMessage, err.Error()))
	emit(ctx, otellog.SeverityWarn, msg, attrs...)
}
