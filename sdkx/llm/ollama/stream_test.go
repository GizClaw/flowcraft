package ollama

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestOllamaStreamMessage_MessageAccumulates(t *testing.T) {
	chunks := []chatResponse{
		{Message: chatMessage{Content: "Hello"}, Done: false},
		{Message: chatMessage{Content: " world"}, Done: false},
		{Message: chatMessage{Content: "!"}, Done: true},
	}

	body := buildJSONStream(chunks)
	span := noop.Span{}
	s := newStreamMessage(context.Background(), span, "test-model", io.NopCloser(body))

	var collected []string
	for s.Next() {
		collected = append(collected, s.Current().Content)
	}
	if s.Err() != nil {
		t.Fatalf("unexpected error: %v", s.Err())
	}
	if len(collected) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(collected))
	}

	msg := s.Message()
	if msg.Role != model.RoleAssistant {
		t.Fatalf("expected assistant role, got %s", msg.Role)
	}
	if msg.Content() != "Hello world!" {
		t.Fatalf("expected accumulated 'Hello world!', got %q", msg.Content())
	}
}

func TestOllamaStreamMessage_EmptyStream(t *testing.T) {
	chunks := []chatResponse{
		{Message: chatMessage{Content: ""}, Done: true},
	}

	body := buildJSONStream(chunks)
	span := noop.Span{}
	s := newStreamMessage(context.Background(), span, "test-model", io.NopCloser(body))

	if s.Next() {
		t.Fatal("expected no chunks from empty stream")
	}

	msg := s.Message()
	if msg.Content() != "" {
		t.Fatalf("expected empty content, got %q", msg.Content())
	}
}

func TestOllamaStreamMessage_Usage(t *testing.T) {
	chunks := []chatResponse{
		{Message: chatMessage{Content: "hi"}, Done: false},
		{Message: chatMessage{Content: ""}, Done: true, PromptEvalCount: 10, EvalCount: 5},
	}

	body := buildJSONStream(chunks)
	span := noop.Span{}
	s := newStreamMessage(context.Background(), span, "test-model", io.NopCloser(body))

	for s.Next() {
	}

	usage := s.Usage()
	if usage.InputTokens != 10 {
		t.Fatalf("expected 10 input tokens, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 5 {
		t.Fatalf("expected 5 output tokens, got %d", usage.OutputTokens)
	}
}

func buildJSONStream(chunks []chatResponse) *strings.Reader {
	var sb strings.Builder
	for _, c := range chunks {
		data, _ := json.Marshal(c)
		sb.Write(data)
		sb.WriteByte('\n')
	}
	return strings.NewReader(sb.String())
}
