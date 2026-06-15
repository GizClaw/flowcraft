package chat

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"

	oai "github.com/openai/openai-go"
)

func TestConvertMessages_DataPartUsesOpenAITextPart(t *testing.T) {
	msgs, err := convertMessages([]llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{{
			Type: llm.PartData,
			Data: &llm.DataRef{
				MimeType: "application/vnd.flowcraft.snapshot+json",
				Value:    map[string]any{"k": "v"},
			},
		}},
	}})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}

	texts := openAIUserTextParts(t, msgs)
	if len(texts) != 1 {
		t.Fatalf("got %d text parts, want 1: %#v", len(texts), texts)
	}
	text := texts[0]
	for _, want := range []string{
		"OpenAI input data",
		"MIME type: application/vnd.flowcraft.snapshot+json",
		"JSON:\n" + `{"k":"v"}`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("data part text missing %q: %q", want, text)
		}
	}
}

func TestConvertMessages_DataPartDefaultsMimeType(t *testing.T) {
	msgs, err := convertMessages([]llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{{
			Type: llm.PartData,
			Data: &llm.DataRef{Value: map[string]any{"ok": true}},
		}},
	}})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	texts := openAIUserTextParts(t, msgs)
	if len(texts) != 1 || !strings.Contains(texts[0], "MIME type: application/json") {
		t.Fatalf("empty mime type should default to application/json: %#v", texts)
	}
}

func TestConvertMessages_ImagePartBase64UsesDataURL(t *testing.T) {
	msgs, err := convertMessages([]llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{{
			Type: llm.PartImage,
			Image: &llm.MediaRef{
				Base64:    "aGVsbG8=",
				MediaType: "image/png",
			},
		}},
	}})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}

	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "image_url") || !strings.Contains(body, "data:image/png;base64,aGVsbG8=") {
		t.Fatalf("base64 image should render as image_url data URL: %s", body)
	}
}

func TestConvertMessages_UserUnsupportedPartValidation(t *testing.T) {
	_, err := convertMessages([]llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{{
			Type:  llm.PartAudio,
			Audio: &llm.MediaRef{Base64: "AA==", MediaType: "audio/mpeg"},
		}},
	}})
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
	for _, want := range []string{"openai chat", "user", string(llm.PartAudio)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validation error missing %q: %v", want, err)
		}
	}
}

func TestConvertMessages_DataPartPreservesAssistantRole(t *testing.T) {
	msgs, err := convertMessages([]llm.Message{{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "before"},
			{Type: llm.PartData, Data: &llm.DataRef{Value: map[string]any{"state": "kept"}}},
			{Type: llm.PartText, Text: "after"},
		},
	}})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"before\\n\\nOpenAI input data",
		`{\"state\":\"kept\"}`,
		"\\n\\nafter",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("converted messages missing %q: %s", want, body)
		}
	}
}

func TestConvertMessages_SystemPartDataValidation(t *testing.T) {
	_, err := convertMessages([]llm.Message{{
		Role: llm.RoleSystem,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "rules"},
			{Type: llm.PartData, Data: &llm.DataRef{Value: map[string]any{"k": "v"}}},
		},
	}})
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestGenerate_DataPartMarshalErrorIsValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Errorf("request should not be sent after data part validation fails")
	}))
	defer srv.Close()

	c, err := New("test-model", "test-key", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msgs := []llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{{
			Type: llm.PartData,
			Data: &llm.DataRef{Value: map[string]any{"bad": math.NaN()}},
		}},
	}}

	_, _, err = c.Generate(context.Background(), msgs)
	if !errdefs.IsValidation(err) {
		t.Fatalf("Generate error = %v, want Validation", err)
	}
	_, err = c.GenerateStream(context.Background(), msgs)
	if !errdefs.IsValidation(err) {
		t.Fatalf("GenerateStream error = %v, want Validation", err)
	}
}

func TestGenerateStream_FallbackClassifiesRetryError(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request %d: %v", requests, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-should-retry", "false")
		switch requests {
		case 1:
			if _, ok := got["stream_options"]; !ok {
				t.Fatalf("first request should include stream_options: %#v", got)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"unsupported_param","message":"stream_options unsupported"}}`))
		case 2:
			if _, ok := got["stream_options"]; ok {
				t.Fatalf("retry request should omit stream_options: %#v", got)
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"second failure"}}`))
		default:
			t.Fatalf("unexpected retry request %d", requests)
		}
	}))
	defer srv.Close()

	c, err := New("test-model", "test-key", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.GenerateStream(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if !errdefs.IsUnauthorized(err) {
		t.Fatalf("GenerateStream error = %v, want retry Unauthorized classification", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func openAIUserTextParts(t *testing.T, msgs []oai.ChatCompletionMessageParamUnion) []string {
	t.Helper()
	if len(msgs) != 1 || msgs[0].OfUser == nil {
		t.Fatalf("expected one user message, got %#v", msgs)
	}
	raw := msgs[0].GetContent().AsAny()
	parts, ok := raw.(*[]oai.ChatCompletionContentPartUnionParam)
	if !ok {
		t.Fatalf("user content = %T, want content part array", raw)
	}
	texts := make([]string, 0, len(*parts))
	for i, part := range *parts {
		text := part.GetText()
		if text == nil {
			t.Fatalf("part %d is not a text content part: %#v", i, part)
		}
		texts = append(texts, *text)
	}
	return texts
}
