package middleware

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/tool"
)

func TestRedact_RedactsResultKeepsArguments(t *testing.T) {
	var sawArgs json.RawMessage
	reg := catalogWith(tool.TextTool(message.ToolDefinition{Name: "secret"},
		func(_ context.Context, args string) (string, error) {
			sawArgs = json.RawMessage(args)
			return "token=abc123 and keep=ok", nil
		}))
	exec := tool.NewExecutor(reg, Redact(RedactRule{
		Pattern: regexp.MustCompile(`abc123`),
	}))

	res := exec.Execute(context.Background(), message.ToolCall{
		ID: "c1", Name: "secret", Arguments: json.RawMessage(`{"token":"abc123"}`),
	})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content.Text())
	}
	if strings.Contains(res.Content.Text(), "abc123") {
		t.Errorf("result still contains secret: %q", res.Content.Text())
	}
	if !strings.Contains(res.Content.Text(), "[REDACTED]") {
		t.Errorf("result has no redaction marker: %q", res.Content.Text())
	}
	if got := string(sawArgs); !strings.Contains(got, "abc123") {
		t.Errorf("tool should receive original arguments for execution, got %s", got)
	}
}

func TestRedact_CustomReplacement(t *testing.T) {
	reg := catalogWith(tool.TextTool(message.ToolDefinition{Name: "card"},
		func(_ context.Context, _ string) (string, error) {
			return "card 4111-1111-1111-1111 ok", nil
		}))
	exec := tool.NewExecutor(reg, Redact(RedactRule{
		Pattern:     regexp.MustCompile(`\d{4}-\d{4}-\d{4}-\d{4}`),
		Replacement: "<card>",
	}))
	res := exec.Execute(context.Background(), call("card"))
	if strings.Contains(res.Content.Text(), "4111") {
		t.Errorf("content still contains card number: %q", res.Content.Text())
	}
	if !strings.Contains(res.Content.Text(), "<card>") {
		t.Errorf("content has no custom replacement: %q", res.Content.Text())
	}
}

func TestRedact_WithAuditRedactedRecord(t *testing.T) {
	var mu sync.Mutex
	var recorded []AuditRecord
	sink := AuditSinkFunc(func(_ context.Context, rec AuditRecord) {
		mu.Lock()
		recorded = append(recorded, rec)
		mu.Unlock()
	})
	reg := catalogWith(tool.TextTool(message.ToolDefinition{Name: "secret"},
		func(_ context.Context, args string) (string, error) {
			return "token=abc123", nil
		}))
	exec := tool.NewExecutor(reg,
		Redact(RedactRule{Pattern: regexp.MustCompile(`abc123`)}),
		AuditRedacted(sink, RedactRule{Pattern: regexp.MustCompile(`abc123`)}),
	)

	call := message.ToolCall{
		ID: "c1", Name: "secret", Arguments: json.RawMessage(`{"token":"abc123"}`),
	}
	if res := exec.Execute(context.Background(), call); res.IsError {
		t.Fatalf("unexpected error: %q", res.Content.Text())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != 1 {
		t.Fatalf("audit records = %d, want 1", len(recorded))
	}
	rec := recorded[0]
	if strings.Contains(string(rec.Call.Arguments), "abc123") {
		t.Errorf("audit call arguments are not redacted: %s", rec.Call.Arguments)
	}
	if strings.Contains(rec.Result.Content.Text(), "abc123") {
		t.Errorf("audit result content is not redacted: %q", rec.Result.Content.Text())
	}
}

func TestAuditRedacted_ModelSeesOriginalAuditRedacted(t *testing.T) {
	var recorded []AuditRecord
	sink := AuditSinkFunc(func(_ context.Context, rec AuditRecord) {
		recorded = append(recorded, rec)
	})
	reg := catalogWith(tool.TextTool(message.ToolDefinition{Name: "secret"},
		func(_ context.Context, _ string) (string, error) {
			return "token=abc123", nil
		}))
	exec := tool.NewExecutor(reg,
		AuditRedacted(sink, RedactRule{
			Pattern: regexp.MustCompile(`abc123`),
		}),
	)

	res := exec.Execute(context.Background(), message.ToolCall{
		ID: "c1", Name: "secret", Arguments: json.RawMessage(`{"token":"abc123"}`),
	})
	if !strings.Contains(res.Content.Text(), "abc123") {
		t.Errorf("model-facing content was redacted, want original: %q", res.Content.Text())
	}
	if len(recorded) != 1 {
		t.Fatalf("audit records = %d, want 1", len(recorded))
	}
	if strings.Contains(recorded[0].Result.Content.Text(), "abc123") {
		t.Errorf("audit content is not redacted: %q", recorded[0].Result.Content.Text())
	}
	if strings.Contains(string(recorded[0].Call.Arguments), "abc123") {
		t.Errorf("audit arguments are not redacted: %s", recorded[0].Call.Arguments)
	}
}

func TestRedact_NilPatternPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for nil pattern")
		}
	}()
	Redact(RedactRule{Replacement: "x"})
}

func TestRedact_CoversTextDataFileAndMediaURLParts(t *testing.T) {
	inline, err := media.NewImageBytes([]byte("abc123"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	linked, err := media.NewImageURL("https://cdn.example.com/abc123.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	data, err := message.NewJSONContent([]byte(`{"token":"abc123","keep":"ok"}`))
	if err != nil {
		t.Fatalf("NewJSONContent: %v", err)
	}
	secret := tool.FuncTool(message.ToolDefinition{Name: "secret"},
		func(_ context.Context, _ string) (message.Content, error) {
			return message.Content{Parts: []message.Part{
				message.TextPart{Text: "token=abc123 keep=ok"},
				data.Parts[0],
				message.FilePart{
					URI:  "https://files.example.com/a?token=abc123",
					Name: "abc123.txt",
				},
				message.ImagePart{Source: inline},
				message.ImagePart{Source: linked},
			}}, nil
		})
	exec := tool.NewExecutor(catalogWith(secret),
		Redact(RedactRule{Pattern: regexp.MustCompile(`abc123`), Replacement: "redacted"}))

	res := exec.Execute(context.Background(), call("secret"))
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content.Text())
	}
	if len(res.Content.Parts) != 5 {
		t.Fatalf("parts = %d, want 5", len(res.Content.Parts))
	}
	if text := res.Content.Parts[0].(message.TextPart).Text; strings.Contains(text, "abc123") || !strings.Contains(text, "redacted") {
		t.Fatalf("text part = %q, want the secret replaced", text)
	}
	redacted, ok := res.Content.Parts[1].(message.DataPart)
	if !ok {
		t.Fatalf("part 1 = %T, want message.DataPart", res.Content.Parts[1])
	}
	if strings.Contains(string(redacted.Value), "abc123") || !strings.Contains(string(redacted.Value), "redacted") {
		t.Fatalf("data part = %s, want the secret replaced", redacted.Value)
	}
	if err := redacted.Validate(); err != nil {
		t.Fatalf("redacted data part must stay a JSON object: %v", err)
	}
	file := res.Content.Parts[2].(message.FilePart)
	if strings.Contains(file.URI, "abc123") || strings.Contains(file.Name, "abc123") {
		t.Fatalf("file part = %#v, want the secret replaced", file)
	}
	if got := string(res.Content.Parts[3].(message.ImagePart).Source.Bytes()); got != "abc123" {
		t.Fatalf("inline media = %q, want encoded bytes untouched", got)
	}
	if got := res.Content.Parts[4].(message.ImagePart).Source.URL(); strings.Contains(got, "abc123") {
		t.Fatalf("media URL = %q, want the secret replaced", got)
	}
}

// TestRedact_CoversToolCallResultAndReasoningParts covers the part kinds a
// tool result can carry beyond text and media: a nested tool call's arguments,
// a nested tool result's own content, and a reasoning trace. All of them are
// model-visible text, so all of them must go through the rules.
func TestRedact_CoversToolCallResultAndReasoningParts(t *testing.T) {
	secret := tool.FuncTool(message.ToolDefinition{Name: "secret"},
		func(_ context.Context, _ string) (message.Content, error) {
			return message.Content{Parts: []message.Part{
				message.ToolCallPart{Call: message.ToolCall{
					ID:        "c1",
					Name:      "http",
					Arguments: json.RawMessage(`{"token":"abc123","keep":"ok"}`),
				}},
				message.ToolResultPart{Result: message.NewTextToolResult(
					"c2", "token=abc123",
				)},
				message.ReasoningPart{Text: "token=abc123", Signature: "sig-abc123"},
			}}, nil
		})
	exec := tool.NewExecutor(catalogWith(secret),
		Redact(RedactRule{Pattern: regexp.MustCompile(`abc123`), Replacement: "redacted"}))

	res := exec.Execute(context.Background(), call("secret"))
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content.Text())
	}
	if len(res.Content.Parts) != 3 {
		t.Fatalf("parts = %d, want 3", len(res.Content.Parts))
	}
	callPart, ok := res.Content.Parts[0].(message.ToolCallPart)
	if !ok {
		t.Fatalf("part 0 = %T, want message.ToolCallPart", res.Content.Parts[0])
	}
	if strings.Contains(string(callPart.Call.Arguments), "abc123") ||
		!strings.Contains(string(callPart.Call.Arguments), "redacted") {
		t.Fatalf("call arguments = %s, want the secret replaced",
			callPart.Call.Arguments)
	}
	if err := callPart.Validate(); err != nil {
		t.Fatalf("redacted call must stay a JSON object: %v", err)
	}
	resultPart, ok := res.Content.Parts[1].(message.ToolResultPart)
	if !ok {
		t.Fatalf("part 1 = %T, want message.ToolResultPart", res.Content.Parts[1])
	}
	if got := resultPart.Result.Content.Text(); strings.Contains(got, "abc123") {
		t.Fatalf("nested tool result = %q, want the secret replaced", got)
	}
	reasoning, ok := res.Content.Parts[2].(message.ReasoningPart)
	if !ok {
		t.Fatalf("part 2 = %T, want message.ReasoningPart", res.Content.Parts[2])
	}
	if strings.Contains(reasoning.Text, "abc123") {
		t.Fatalf("reasoning text = %q, want the secret replaced", reasoning.Text)
	}
	// The signature is an opaque provider payload: rewriting it would break
	// the round-trip without hiding anything the model reads.
	if reasoning.Signature != "sig-abc123" {
		t.Fatalf("reasoning signature = %q, want it untouched", reasoning.Signature)
	}
}

// TestRedact_AccountsForEveryPartKind pins the coverage contract: every kind
// in the content vocabulary has a deliberate redaction outcome, so a new kind
// cannot be added without deciding whether its text is rewritten. The kinds
// that intentionally keep their payload are inline media (rewriting encoded
// bytes would corrupt them) and the opaque reasoning signature.
func TestRedact_AccountsForEveryPartKind(t *testing.T) {
	inlineImage, err := media.NewImageBytes([]byte("abc123"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	inlineAudio, err := media.NewAudioBytes([]byte("abc123"), "audio/wav")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	inlineVideo, err := media.NewVideoBytes([]byte("abc123"), "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoBytes: %v", err)
	}
	linkedImage, err := media.NewImageURL("https://cdn.example.com/abc123.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}

	for _, kind := range message.PartKinds() {
		t.Run(string(kind), func(t *testing.T) {
			part := partCarryingSecret(t, kind, inlineImage, inlineAudio, inlineVideo, linkedImage)
			redacted := newRedactor(
				RedactRule{Pattern: regexp.MustCompile(`abc123`)},
			).Content(message.Content{Parts: []message.Part{part}})

			encoded, err := json.Marshal(redacted)
			if err != nil {
				t.Fatalf("marshal redacted content: %v", err)
			}
			switch kind {
			case message.PartImage, message.PartAudio, message.PartVideo:
				// Inline bytes are encoded payloads, not text: the redactor
				// documents that it leaves them alone.
				if !strings.Contains(string(encoded), "abc123") &&
					!strings.Contains(string(encoded), base64.StdEncoding.EncodeToString([]byte("abc123"))) {
					t.Fatalf("inline media lost its payload: %s", encoded)
				}
			default:
				if strings.Contains(string(encoded), "abc123") {
					t.Fatalf("part kind %q kept the secret: %s", kind, encoded)
				}
			}
			if err := redacted.Validate(); err != nil {
				t.Fatalf("redacted content is invalid: %v", err)
			}
		})
	}
}

// partCarryingSecret builds one part of kind carrying "abc123" wherever the
// kind can hold text. Kinds the redactor cannot rewrite keep their encoded
// payload; the caller decides what that means.
func partCarryingSecret(
	t *testing.T,
	kind message.PartKind,
	image media.ImageSource,
	audio media.AudioSource,
	video media.VideoSource,
	linkedImage media.ImageSource,
) message.Part {
	t.Helper()
	_ = linkedImage
	switch kind {
	case message.PartText:
		return message.TextPart{Text: "token=abc123"}
	case message.PartImage:
		return message.ImagePart{Source: image}
	case message.PartAudio:
		return message.AudioPart{Source: audio}
	case message.PartVideo:
		return message.VideoPart{Source: video}
	case message.PartFile:
		return message.FilePart{URI: "https://files.example.com/a?token=abc123", Name: "abc123.txt"}
	case message.PartData:
		content, err := message.NewJSONContent([]byte(`{"token":"abc123"}`))
		if err != nil {
			t.Fatalf("NewJSONContent: %v", err)
		}
		return content.Parts[0]
	case message.PartToolCall:
		return message.ToolCallPart{Call: message.ToolCall{
			ID:        "c1",
			Name:      "http",
			Arguments: json.RawMessage(`{"token":"abc123"}`),
		}}
	case message.PartToolResult:
		return message.ToolResultPart{Result: message.NewTextToolResult("c1", "token=abc123")}
	case message.PartReasoning:
		return message.ReasoningPart{Text: "token=abc123", Signature: "sig"}
	default:
		t.Fatalf(
			"part kind %q has no redaction expectation: decide whether its text is rewritten",
			kind,
		)
		return nil
	}
}
