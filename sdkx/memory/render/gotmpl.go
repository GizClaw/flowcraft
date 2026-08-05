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

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
)

const templateName = "memory-context"

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

var _ sdkmemory.ContextRenderer = (*GoTemplate)(nil)

func NewGoTemplate(settings GoTemplateSettings) (*GoTemplate, error) {
	if settings.MaxChars < 0 {
		return nil, errors.New("memory render: max_chars must not be negative")
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
	var output bytes.Buffer
	if err := renderer.template.Execute(&output, result); err != nil {
		return message.Content{}, fmt.Errorf("memory render: execute Go template: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return message.Content{}, err
	}
	text := output.String()
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
