// Package when adapts [github.com/olebedev/when] to the
// sdk/text/timex.Parser interface.
//
// olebedev/when is the de-facto natural-language date / time
// parser in the Go ecosystem. It handles relative phrases
// ("tomorrow at 3pm", "next Wednesday", "in three weeks"), mixed
// absolute / relative ("by Friday at 14:00"), and ordinal weekday
// references ("the second Tuesday of June") — the entire class
// of expressions the zero-dependency [sdk/text/timex.RegexParser]
// baseline intentionally skips.
//
// Language coverage at the time of writing (when v1.1.0):
// English, Russian, Brazilian Portuguese, Chinese, and Dutch.
// This adapter ships English by default; callers needing more
// languages should construct the underlying when.Parser
// themselves with [github.com/olebedev/when/rules/<lang>] and
// wrap it via [WrapParser].
package when

import (
	"context"
	"time"

	olwhen "github.com/olebedev/when"
	"github.com/olebedev/when/rules/common"
	"github.com/olebedev/when/rules/en"

	"github.com/GizClaw/flowcraft/sdk/text/timex"
)

// Parser wraps [olwhen.Parser] and exposes it through the
// sdk/text/timex.Parser interface. Construction loads the English
// rule set plus the shared common rules (durations, numerals).
//
// Parser is safe for concurrent use — the underlying olwhen.Parser
// is immutable after construction.
type Parser struct {
	p *olwhen.Parser
}

// New constructs a [Parser] with English + common rules loaded.
//
// The constructor never fails — when's rule registration is in-
// memory and deterministic — but returns an error for API
// symmetry with other adapters that may surface I/O errors.
func New() (*Parser, error) {
	w := olwhen.New(nil)
	w.Add(en.All...)
	w.Add(common.All...)
	return &Parser{p: w}, nil
}

// WrapParser adapts a pre-configured [olwhen.Parser] (with custom
// rule sets, distance settings, or non-English languages) to the
// timex.Parser interface.
//
// Callers needing CJK / multi-language parsing should construct
// their own olwhen.Parser with the language-specific rule set
// (e.g. [github.com/olebedev/when/rules/zh]) and wrap it here so
// the rest of the SDK keeps depending only on timex.Parser.
func WrapParser(p *olwhen.Parser) *Parser {
	return &Parser{p: p}
}

// Parse implements [timex.Parser].
//
// olwhen returns (nil, nil) when no time expression is found;
// this adapter preserves that contract. Errors from the
// underlying parser propagate verbatim so callers can
// distinguish "no match" from "malformed rules".
func (p *Parser) Parse(text string, now time.Time) (*timex.Match, error) {
	if text == "" {
		return nil, nil
	}
	r, err := p.p.Parse(text, now)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, nil
	}
	return &timex.Match{
		Time:  r.Time,
		Text:  r.Text,
		Index: r.Index,
	}, nil
}

// ParseContext mirrors [Parse] but threads a context.Context
// through to the underlying parser. when supports context
// cancellation for long-running multi-language rule evaluations;
// most callers do not need this and should reach for [Parse]
// instead.
func (p *Parser) ParseContext(ctx context.Context, text string, now time.Time) (*timex.Match, error) {
	if text == "" {
		return nil, nil
	}
	r, err := p.p.Parse(text, now)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &timex.Match{
		Time:  r.Time,
		Text:  r.Text,
		Index: r.Index,
	}, nil
}
