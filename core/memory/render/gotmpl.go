// Package render projects structured memory context into prompt content.
package render

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"
	"text/template"
	"unicode/utf8"

	sdkmemory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/message"
)

const templateName = "memory-context"

// maxSafeMaxChars keeps the byte budget in Render (maxChars*utf8.UTFMax)
// inside int range so an absurdly large configuration cannot overflow
// into a negative budget that silently disables the bound.
const maxSafeMaxChars = (int(^uint(0)>>1) - utf8.UTFMax) / utf8.UTFMax

// errOutputTooLarge aborts template execution as soon as the bounded
// writer hits its byte budget, so a runaway template cannot first
// allocate the whole expansion in memory.
var errOutputTooLarge = errors.New("memory render: output exceeds byte budget")

//go:embed default.gotmpl
var defaultGoTemplate string

// DefaultGoTemplate returns the embedded default template. It renders recalled
// values as explicitly untrusted reference data and escapes item text and
// titles so recalled content cannot close its structural tags.
func DefaultGoTemplate() string { return defaultGoTemplate }

// GoTemplateSettings configures the deterministic text/template renderer.
// An empty Template selects DefaultGoTemplate. MaxChars is an optional hard
// limit on the rendered Unicode rune count; zero means no additional limit.
type GoTemplateSettings struct {
	Template string `yaml:"template,omitempty"`
	MaxChars int    `yaml:"max_chars,omitempty"`
}

// GoTemplate renders one ContextResult into a single TextPart.
type GoTemplate struct {
	template *template.Template
	maxChars int
}

// boundedBuffer stores template output but stops writing once maxBytes
// is reached. Each UTF-8 rune occupies at most utf8.UTFMax bytes, so
// maxChars*utf8.UTFMax is a safe upper byte bound for a render that is
// supposed to stay under maxChars runes.
type boundedBuffer struct {
	buf      bytes.Buffer
	maxBytes int
	tooLarge bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.maxBytes > 0 && (b.tooLarge || b.buf.Len()+len(p) > b.maxBytes) {
		b.tooLarge = true
		return 0, errOutputTooLarge
	}
	return b.buf.Write(p)
}

var _ sdkmemory.ContextRenderer = (*GoTemplate)(nil)

func NewGoTemplate(settings GoTemplateSettings) (*GoTemplate, error) {
	if settings.MaxChars < 0 {
		return nil, errors.New("memory render: max_chars must not be negative")
	}
	if settings.MaxChars > maxSafeMaxChars {
		return nil, errors.New("memory render: max_chars is too large")
	}
	source := settings.Template
	if strings.TrimSpace(source) == "" {
		source = defaultGoTemplate
	}
	compiled, err := template.New(templateName).
		Option("missingkey=error").
		Funcs(template.FuncMap{
			"contentJSON": contentJSON,
			"contentText": func(content message.Content) string { return content.Text() },
			"score":       func(value float64) string { return fmt.Sprintf("%.3f", value) },
			"xml":         html.EscapeString,
		}).
		Parse(source)
	if err != nil {
		return nil, fmt.Errorf("memory render: compile Go template: %w", err)
	}
	return &GoTemplate{template: compiled, maxChars: settings.MaxChars}, nil
}

func (renderer *GoTemplate) Render(ctx context.Context, result sdkmemory.ContextResult) (message.Content, error) {
	if renderer == nil || renderer.template == nil {
		return message.Content{}, errors.New("memory render: Go template renderer is incomplete")
	}
	if ctx == nil {
		return message.Content{}, errors.New("memory render: context is required")
	}
	if err := ctx.Err(); err != nil {
		return message.Content{}, err
	}
	result = result.Clone()
	var output boundedBuffer
	if renderer.maxChars > 0 {
		output.maxBytes = renderer.maxChars*utf8.UTFMax + utf8.UTFMax
	}
	if err := renderer.template.Execute(&output, result); err != nil {
		if errors.Is(err, errOutputTooLarge) {
			return message.Content{}, fmt.Errorf(
				"memory render: output exceeds max_chars %d", renderer.maxChars)
		}
		return message.Content{}, fmt.Errorf("memory render: execute Go template: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return message.Content{}, err
	}
	text := output.buf.String()
	if renderer.maxChars > 0 && utf8.RuneCountInString(text) > renderer.maxChars {
		return message.Content{}, fmt.Errorf(
			"memory render: output has %d chars, exceeds max_chars %d",
			utf8.RuneCountInString(text), renderer.maxChars,
		)
	}
	return message.Content{Parts: []message.Part{message.TextPart{Text: text}}}, nil
}

func contentJSON(content message.Content) (string, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("encode content: %w", err)
	}
	return string(encoded), nil
}
