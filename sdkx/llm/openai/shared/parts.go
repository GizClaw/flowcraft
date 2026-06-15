package shared

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

func ValidateSystemTextParts(parts []llm.Part) error {
	for _, p := range parts {
		if p.Type != llm.PartText {
			return errdefs.Validationf("openai: system message supports text parts only, got %s", p.Type)
		}
	}
	return nil
}

func TextContent(parts []llm.Part) (string, error) {
	var b strings.Builder
	needsBoundary := false
	for _, p := range parts {
		switch p.Type {
		case llm.PartText:
			if needsBoundary && p.Text != "" {
				EnsureTextBoundary(&b)
			}
			b.WriteString(p.Text)
			needsBoundary = false
		case llm.PartData:
			if p.Data == nil {
				continue
			}
			text, err := FormatDataPartText(p.Data)
			if err != nil {
				return "", err
			}
			EnsureTextBoundary(&b)
			b.WriteString(text)
			needsBoundary = true
		}
	}
	return b.String(), nil
}

func FormatDataPartText(data *llm.DataRef) (string, error) {
	b, err := json.Marshal(data.Value)
	if err != nil {
		return "", errdefs.Validationf("openai: marshal data part: %v", err)
	}
	mime := strings.TrimSpace(data.MimeType)
	if mime == "" {
		mime = "application/json"
	}
	return fmt.Sprintf("OpenAI input data\nMIME type: %s\nJSON:\n%s", mime, string(b)), nil
}

func EnsureTextBoundary(b *strings.Builder) {
	if b.Len() == 0 {
		return
	}
	s := b.String()
	switch {
	case strings.HasSuffix(s, "\n\n"):
		return
	case strings.HasSuffix(s, "\n"):
		b.WriteByte('\n')
	default:
		b.WriteString("\n\n")
	}
}
