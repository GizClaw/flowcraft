package middleware

import (
	"context"
	"regexp"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// DefaultRedaction is the replacement applied when a rule does not
// specify one.
const DefaultRedaction = "[REDACTED]"

// RedactRule is one regex-based redaction: every match of Pattern is
// replaced by Replacement (DefaultRedaction when empty). Patterns use
// Go's regexp (RE2) syntax; the replacement string may reference
// capture groups with ${name} or $1.
type RedactRule struct {
	Pattern     *regexp.Regexp
	Replacement string
}

// Redact rewrites tool result content through the given rules before it
// is returned to the model. Call arguments are deliberately left
// untouched: the tool needs the real arguments to execute, and the
// audit path has its own redacting sink ([AuditRedacted]).
func Redact(rules ...RedactRule) tool.Middleware {
	red := newRedactor(rules...)
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.Call) message.Result {
			return red.Result(next(ctx, call))
		}
	}
}

// RedactPatterns is the shorthand form of [Redact]: each pattern is
// replaced with DefaultRedaction.
func RedactPatterns(patterns ...string) tool.Middleware {
	rules := make([]RedactRule, 0, len(patterns))
	for _, pattern := range patterns {
		rules = append(rules, RedactRule{Pattern: regexp.MustCompile(pattern)})
	}
	return Redact(rules...)
}

// AuditRedacted is [Audit] over a redacting wrapper: the audit record
// receives redacted copies of both call arguments and result content,
// while the model and the tool continue to see the originals. Pair it
// with [Redact] when the model-facing result must also be stripped:
//
//	exec := tool.NewExecutor(reg,
//	    middleware.Redact(rules...),
//	    middleware.AuditRedacted(sink, rules...),
//	)
func AuditRedacted(sink AuditSink, rules ...RedactRule) tool.Middleware {
	if sink == nil {
		panic("middleware.AuditRedacted: sink is nil")
	}
	red := newRedactor(rules...)
	return Audit(redactingSink{sink: sink, redactor: red})
}

// redactingSink redacts each record before handing it to the wrapped
// sink.
type redactingSink struct {
	sink     AuditSink
	redactor *redactor
}

func (s redactingSink) Record(ctx context.Context, rec AuditRecord) {
	rec.Call.Arguments = s.redactor.Bytes(rec.Call.Arguments)
	rec.Result = s.redactor.Result(rec.Result)
	s.sink.Record(ctx, rec)
}

type redactor struct {
	rules []RedactRule
}

func newRedactor(rules ...RedactRule) *redactor {
	for _, rule := range rules {
		if rule.Pattern == nil {
			panic("middleware: redaction rule has a nil pattern")
		}
	}
	return &redactor{rules: rules}
}

func (r *redactor) String(s string) string {
	if len(r.rules) == 0 {
		return s
	}
	for _, rule := range r.rules {
		replacement := rule.Replacement
		if replacement == "" {
			replacement = DefaultRedaction
		}
		s = rule.Pattern.ReplaceAllString(s, replacement)
	}
	return s
}

func (r *redactor) Bytes(b []byte) []byte {
	if len(r.rules) == 0 {
		return b
	}
	out := b
	for _, rule := range r.rules {
		replacement := rule.Replacement
		if replacement == "" {
			replacement = DefaultRedaction
		}
		out = rule.Pattern.ReplaceAll(out, []byte(replacement))
	}
	return out
}

func (r *redactor) Result(res message.Result) message.Result {
	res.Content = r.String(res.Content)
	return res
}
