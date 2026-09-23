package summary

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

type fakeGenerate struct {
	reply    string
	err      error
	requests []inference.GenerateRequest
}

func (runtime *fakeGenerate) Generate(_ context.Context, _ model.ModelRef, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	runtime.requests = append(runtime.requests, request)
	if runtime.err != nil {
		return inference.GenerateResponse{}, runtime.err
	}
	return inference.GenerateResponse{Message: coremessage.NewTextMessage(coremessage.RoleAssistant, runtime.reply)}, nil
}

func TestLLMSummarizerRendersInputsAndBoundsOutput(t *testing.T) {
	runtime := &fakeGenerate{reply: "  On 7 May 2023, Caroline joined a support group.  "}
	summarizer, err := NewLLMSummarizer(runtime, model.ModelRef{ID: model.ModelID{Provider: "deepseek", Name: "deepseek-chat"}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := summarizer.Summarize(context.Background(), SummarizeRequest{
		Level: 0, Texts: []string{"Caroline went to a support group.", "It was on 7 May 2023."}, Topics: []string{"Caroline"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "On 7 May 2023, Caroline joined a support group." {
		t.Fatalf("summary = %q", text)
	}
	prompt := runtime.requests[0].Input.Content.Text()
	if !strings.Contains(prompt, "support group") || !strings.Contains(prompt, "Topics: Caroline") {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestLLMSummarizerRejectsEmptyAndPropagatesErrors(t *testing.T) {
	summarizer, err := NewLLMSummarizer(&fakeGenerate{reply: "  "}, model.ModelRef{ID: model.ModelID{Provider: "p", Name: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := summarizer.Summarize(context.Background(), SummarizeRequest{Texts: []string{"x"}}); err == nil {
		t.Fatal("empty summary accepted")
	}
	failing, err := NewLLMSummarizer(&fakeGenerate{err: errors.New("boom")}, model.ModelRef{ID: model.ModelID{Provider: "p", Name: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.Summarize(context.Background(), SummarizeRequest{Texts: []string{"x"}}); err == nil {
		t.Fatal("generate error swallowed")
	}
	if _, err := NewLLMSummarizer(nil, model.ModelRef{}); err == nil {
		t.Fatal("incomplete summarizer accepted")
	}
}
