package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/tool"
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
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
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
//	exec := NewExecutor(reg,
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

func (r *redactor) Result(res message.ToolResult) message.ToolResult {
	res.Content = r.Content(res.Content)
	return res
}

// Content rewrites every text-bearing part through the rules: text
// parts, file references, structured data, and URL-backed media
// sources. Inline media holds encoded bytes, which rewriting in place
// would corrupt, so it passes through untouched.
func (r *redactor) Content(content message.Content) message.Content {
	if len(r.rules) == 0 || len(content.Parts) == 0 {
		return content
	}
	out := content.Clone()
	for i, part := range out.Parts {
		switch value := part.(type) {
		case message.TextPart:
			value.Text = r.String(value.Text)
			out.Parts[i] = value
		case message.DataPart:
			out.Parts[i] = r.dataPart(value)
		case message.FilePart:
			value.URI = r.String(value.URI)
			value.Name = r.String(value.Name)
			out.Parts[i] = value
		case message.ImagePart, message.AudioPart, message.VideoPart:
			out.Parts[i] = r.mediaPart(value)
		}
	}
	return out
}

// dataPart rewrites the JSON payload of a data part. A replacement that
// breaks the JSON structure must not resurrect the original secret, so
// the redacted bytes fall back to a text part — which is how drivers
// without a structured surface render data anyway.
func (r *redactor) dataPart(part message.DataPart) message.Part {
	redacted := r.Bytes(part.Value)
	if bytes.Equal(redacted, part.Value) {
		return part
	}
	part.Value = json.RawMessage(redacted)
	if err := part.Validate(); err != nil {
		return message.TextPart{Text: string(redacted)}
	}
	return part
}

// mediaPart rewrites the URL of a URL-backed media source. An inline
// source carries encoded bytes and passes through untouched.
func (r *redactor) mediaPart(part message.Part) message.Part {
	var rawURL, mediaType string
	switch value := part.(type) {
	case message.ImagePart:
		if value.Source.Kind() != media.SourceURL {
			return part
		}
		rawURL, mediaType = value.Source.URL(), value.Source.MediaType()
	case message.AudioPart:
		if value.Source.Kind() != media.SourceURL {
			return part
		}
		rawURL, mediaType = value.Source.URL(), value.Source.MediaType()
	case message.VideoPart:
		if value.Source.Kind() != media.SourceURL {
			return part
		}
		rawURL, mediaType = value.Source.URL(), value.Source.MediaType()
	default:
		return part
	}
	redacted := r.String(rawURL)
	if redacted == rawURL {
		return part
	}
	switch value := part.(type) {
	case message.ImagePart:
		source, err := media.NewImageURL(redacted, mediaType)
		if err != nil {
			return message.TextPart{Text: redacted}
		}
		value.Source = source
		return value
	case message.AudioPart:
		source, err := media.NewAudioURL(redacted, mediaType)
		if err != nil {
			return message.TextPart{Text: redacted}
		}
		value.Source = source
		return value
	case message.VideoPart:
		source, err := media.NewVideoURL(redacted, mediaType)
		if err != nil {
			return message.TextPart{Text: redacted}
		}
		value.Source = source
		return value
	}
	return part
}
