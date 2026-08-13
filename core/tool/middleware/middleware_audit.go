package middleware

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// AuditRecord is one dispatched call: what was asked, what came
// back, and how long the inner chain took. Sink receives records
// after every call that reaches this middleware — including
// short-circuit results produced inside it.
type AuditRecord struct {
	Call     message.ToolCall
	Result   message.ToolResult
	Duration time.Duration
}

// AuditSink consumes audit records. Implementations must be safe for
// concurrent use and must not block execution meaningfully; a sink
// that fails handles its own errors (audit never breaks execution).
type AuditSink interface {
	Record(ctx context.Context, rec AuditRecord)
}

// AuditSinkFunc adapts a plain function to AuditSink.
type AuditSinkFunc func(ctx context.Context, rec AuditRecord)

func (f AuditSinkFunc) Record(ctx context.Context, rec AuditRecord) {
	f(ctx, rec)
}

// Audit reports every call and its outcome to sink. Call arguments
// and result content are included verbatim — choose a sink whose
// storage is appropriate for your data classification.
func Audit(sink AuditSink) tool.Middleware {
	if sink == nil {
		panic("middleware.Audit: sink is nil")
	}
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			start := time.Now()
			res := next(ctx, call)
			sink.Record(ctx, AuditRecord{
				Call:     call,
				Result:   res,
				Duration: time.Since(start),
			})
			return res
		}
	}
}
