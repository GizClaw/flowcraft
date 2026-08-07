package main

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/GizClaw/flowcraft/memory/eval/dataset"
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
)

//go:embed prompts/*.tmpl
var promptFS embed.FS

var (
	answerSystemTmpl = mustPromptTemplate("prompts/answer_system.tmpl")
	answerUserTmpl   = mustPromptTemplate("prompts/answer_user.tmpl")
	answerItemTmpl   = mustPromptTemplate("prompts/answer_item.tmpl")
	judgeSystemTmpl  = mustPromptTemplate("prompts/judge_system.tmpl")
	judgeUserTmpl    = mustPromptTemplate("prompts/judge_user.tmpl")

	answerSystem = mustRenderPrompt(answerSystemTmpl)
	judgeSystem  = mustRenderPrompt(judgeSystemTmpl)
)

func mustPromptTemplate(name string) *template.Template {
	return template.Must(template.ParseFS(promptFS, name))
}

func mustRenderPrompt(tmpl *template.Template) string {
	value, err := renderPrompt(tmpl, nil)
	if err != nil {
		panic(err)
	}
	return value
}

func renderPrompt(tmpl *template.Template, data any) (string, error) {
	var buffer bytes.Buffer
	if err := tmpl.Execute(&buffer, data); err != nil {
		return "", fmt.Errorf("render prompt %s: %w", tmpl.Name(), err)
	}
	return buffer.String(), nil
}

// buildAnswerInput renders the question and every recalled item as the user
// turn of the answer request. The instructions live in the system message;
// data parts (eval turn metadata) are stripped, text and image parts are
// preserved so vision-capable answer models see the multimodal evidence.
func buildAnswerInput(question dataset.Question, items []sdkmemory.ContextItem) (message.Content, error) {
	header, err := renderPrompt(answerUserTmpl, struct {
		AskedAt string
		Query   string
	}{question.AskedAt, question.Query})
	if err != nil {
		return message.Content{}, err
	}
	parts := []message.Part{message.TextPart{Text: header}}
	for index, item := range items {
		label, labelErr := renderPrompt(answerItemTmpl, struct {
			Index       int
			SourceClass string
			Kind        string
		}{index + 1, string(item.SourceClass), string(item.Kind)})
		if labelErr != nil {
			return message.Content{}, labelErr
		}
		parts = append(parts, message.TextPart{Text: "\n" + label + "\n"})
		parts = append(parts, sanitizeInferenceContent(item.Content).Parts...)
	}
	return message.Content{Parts: parts}, nil
}

// buildJudgeInput renders the gold answers and prediction as the user turn
// of the judge request. The instructions live in the system message.
func buildJudgeInput(golds []string, prediction string) (message.Content, error) {
	body, err := renderPrompt(judgeUserTmpl, struct {
		Golds      []string
		Prediction string
	}{golds, prediction})
	if err != nil {
		return message.Content{}, err
	}
	return message.Content{Parts: []message.Part{message.TextPart{Text: body}}}, nil
}

// sanitizeInferenceContent keeps only parts models can consume (text and
// image) and drops eval-only data parts.
func sanitizeInferenceContent(content message.Content) message.Content {
	var parts []message.Part
	for _, part := range content.Parts {
		switch part.Kind() {
		case message.PartText, message.PartImage:
			parts = append(parts, part)
		}
	}
	return message.Content{Parts: parts}
}

// generateWithSystem sends one chat turn: a system instruction message plus
// a user input that may carry multimodal parts.
func generateWithSystem(
	ctx context.Context,
	runtime *inference.Runtime,
	model inference.ModelRef,
	system string,
	content message.Content,
) (string, error) {
	response, err := runtime.Generate(ctx, model, inference.GenerateRequest{
		Context: []message.Message{{
			Role:    message.RoleSystem,
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: system}}},
		}},
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: content,
				Intent:  inference.Intent{Text: &inference.TextIntent{}},
			},
		},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(response.Message.Content.Text()), nil
}
