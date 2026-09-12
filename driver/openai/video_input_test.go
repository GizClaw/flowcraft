package openai

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// videoEntry is one chat-surface catalog entry that declares video input, the
// shape a compatible endpoint publishes with spec.wire.video_input.
func videoEntry() catalogEntry {
	entry := catalog["gpt-5.6-sol"]
	entry.dialect.api = apiChat
	entry.dialect.videoInput = true
	entry.capabilities.Inputs = append(
		append([]message.PartKind(nil), entry.capabilities.Inputs...),
		message.PartVideo,
	)
	return entry
}

// chatResponseJSON is the minimal successful Chat Completions body.
const chatResponseJSON = `{
	"id": "chatcmpl_1",
	"object": "chat.completion",
	"choices": [{"index": 0,
		"message": {"role": "assistant", "content": "ok"},
		"finish_reason": "stop"}]
}`

// videoRequest builds a chat request whose current input carries text and one
// video part.
func videoRequest(t *testing.T, source media.VideoSource) inference.GenerateRequest {
	t.Helper()
	request := simpleTextRequest("what happens in this clip?")
	request.Input.Content.Parts = append(
		request.Input.Content.Parts,
		message.VideoPart{Source: source},
	)
	return request
}

// chatContentArray pulls the first message's content array out of a captured
// request body.
func chatContentArray(t *testing.T, body map[string]any) []any {
	t.Helper()
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("messages = %#v, want the compiled array", body["messages"])
	}
	first, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("message 0 = %#v", messages[0])
	}
	content, ok := first["content"].([]any)
	if !ok {
		t.Fatalf("content = %#v, want the content array", first["content"])
	}
	return content
}

// TestChatVideoPartLowersToVideoURL drives the lowering end to end: the SDK's
// content union has no video variant, so the compiler reserves a placeholder
// element and the request replaces it with the video_url object the compatible
// endpoints accept.
func TestChatVideoPartLowersToVideoURL(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		_ *http.Request,
		_ map[string]any,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, chatResponseJSON)
	})
	defer server.Close()

	source, err := media.NewVideoURL("https://cdn.example.com/clip.mp4", "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoURL: %v", err)
	}
	compiled, err := compileChat("gpt-5.6-sol", videoEntry())(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		videoRequest(t, source),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileChat: %v", err)
	}
	if compiled.Report.Rejects(inference.FieldGenerateInputVideo) {
		t.Fatalf("video was rejected: %+v", compiled.Report.Decisions)
	}
	if _, err := transportChatGenerate(testClients(t, server).api)(
		context.Background(), compiled.Wire,
	); err != nil {
		t.Fatalf("transport: %v", err)
	}

	content := chatContentArray(t, capture.body(0))
	if len(content) != 2 {
		t.Fatalf("content = %#v, want a text element and a video element", content)
	}
	text, ok := content[0].(map[string]any)
	if !ok || text["type"] != "text" {
		t.Fatalf("content[0] = %#v, want the text element", content[0])
	}
	video, ok := content[1].(map[string]any)
	if !ok {
		t.Fatalf("content[1] = %#v, want the video element", content[1])
	}
	if video["type"] != "video_url" {
		t.Fatalf("content[1].type = %v, want video_url", video["type"])
	}
	url, ok := video["video_url"].(map[string]any)
	if !ok {
		t.Fatalf("content[1].video_url = %#v", video["video_url"])
	}
	if url["url"] != "https://cdn.example.com/clip.mp4" {
		t.Fatalf("video url = %v", url["url"])
	}
}

// TestChatInlineVideoBecomesDataURI pins the inline shape: an inline video
// travels as a data URI, exactly like an inline image.
func TestChatInlineVideoBecomesDataURI(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		_ *http.Request,
		_ map[string]any,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, chatResponseJSON)
	})
	defer server.Close()

	source, err := media.NewVideoBytes([]byte("clip"), "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoBytes: %v", err)
	}
	compiled, err := compileChat("gpt-5.6-sol", videoEntry())(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		videoRequest(t, source),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileChat: %v", err)
	}
	if _, err := transportChatGenerate(testClients(t, server).api)(
		context.Background(), compiled.Wire,
	); err != nil {
		t.Fatalf("transport: %v", err)
	}

	content := chatContentArray(t, capture.body(0))
	video := content[1].(map[string]any)
	url := video["video_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:video/mp4;base64,") {
		t.Fatalf("inline video url = %q, want a data URI", url)
	}
}

// TestVideoRejectionsAreReported pins every way a video part can fail to ride:
// each one is a ledger rejection naming the field and the reason, never a
// silent omission.
func TestVideoRejectionsAreReported(t *testing.T) {
	urlSource, err := media.NewVideoURL("https://cdn.example.com/clip.mp4", "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoURL: %v", err)
	}
	streamSource, err := media.NewVideoStream[message.Part](
		testVideoStream{},
		"video/mp4",
	)
	if err != nil {
		t.Fatalf("NewVideoStream: %v", err)
	}

	declared := videoEntry()
	undeclared := videoEntry()
	undeclared.capabilities.Inputs = []message.PartKind{
		message.PartText, message.PartData,
	}
	noEndpointFact := videoEntry()
	noEndpointFact.dialect.videoInput = false
	responsesSurface := videoEntry()
	responsesSurface.dialect.api = apiResponses

	assistantVideo := videoRequest(t, urlSource)
	assistantVideo.Context = []message.Message{{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "earlier turn"},
			message.VideoPart{Source: urlSource},
		}},
	}}
	systemVideo := videoRequest(t, urlSource)
	systemVideo.Context = []message.Message{{
		Role: message.RoleSystem,
		Content: message.Content{Parts: []message.Part{
			message.VideoPart{Source: urlSource},
		}},
	}}

	for _, tc := range []struct {
		name    string
		entry   catalogEntry
		request inference.GenerateRequest
		field   inference.FieldID
		reason  string
	}{
		{
			name:    "endpoint does not accept video",
			entry:   noEndpointFact,
			request: videoRequest(t, urlSource),
			field:   inference.FieldGenerateInputVideo,
			reason:  "spec.wire.video_input",
		},
		{
			name:    "model does not declare video",
			entry:   undeclared,
			request: videoRequest(t, urlSource),
			field:   inference.FieldGenerateInputVideo,
			reason:  "model does not accept video input",
		},
		{
			name:    "responses surface has no lowering",
			entry:   responsesSurface,
			request: videoRequest(t, urlSource),
			field:   inference.FieldGenerateInputVideo,
			reason:  "responses surface has no video input lowering",
		},
		{
			name:    "unmaterialized stream source",
			entry:   declared,
			request: videoRequest(t, streamSource),
			field:   inference.FieldGenerateInputVideo,
			reason:  "stream media sources must be materialized",
		},
		{
			name:    "assistant context",
			entry:   declared,
			request: assistantVideo,
			field:   inference.FieldGenerateContextVideo,
			reason:  "chat completions lowers assistant content to text",
		},
		{
			name:    "system context",
			entry:   declared,
			request: systemVideo,
			field:   inference.FieldGenerateContextVideo,
			reason:  "chat completions carries media on user turns only",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := compileChat("gpt-5.6-sol", tc.entry)(
				context.Background(),
				openaiModel("gpt-5.6-sol"),
				tc.request,
				inference.GenerateExecutionUnary,
			)
			if err == nil {
				t.Fatalf("compileChat succeeded, want a rejection: %+v",
					compiled.Report.Decisions)
			}
			decision, ok := decisionFor(compiled.Report.Decisions, tc.field)
			if !ok {
				t.Fatalf("no decision for %q: %+v", tc.field, compiled.Report.Decisions)
			}
			if decision.Disposition != inference.Rejected {
				t.Fatalf("decision = %+v, want Rejected", decision)
			}
			if !strings.Contains(decision.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to mention %q",
					decision.Reason, tc.reason)
			}
		})
	}
}

// TestChatAssistantImageIsRejectedNotDropped covers the silent-loss fix: the
// chat surface lowers assistant content to a plain string, so an assistant
// image used to reach the sink and vanish after the ledger had recorded it as
// carried. It is a rejection now.
func TestChatAssistantImageIsRejectedNotDropped(t *testing.T) {
	image, err := media.NewImageURL("https://cdn.example.com/shot.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	request := simpleTextRequest("go on")
	request.Context = []message.Message{{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "earlier"},
			message.ImagePart{Source: image},
		}},
	}}
	entry := catalog["gpt-5.6-sol"]
	entry.dialect.api = apiChat

	compiled, err := compileChat("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatalf("compileChat succeeded, want a rejection: %+v",
			compiled.Report.Decisions)
	}
	decision, ok := decisionFor(
		compiled.Report.Decisions, inference.FieldGenerateContextImage,
	)
	if !ok || decision.Disposition != inference.Rejected {
		t.Fatalf("image decision = %+v, want Rejected", decision)
	}
	if !strings.Contains(decision.Reason, "assistant content to text") {
		t.Fatalf("reason = %q", decision.Reason)
	}
}

// TestChatVideoKeepsPartOrder pins the index math the patch depends on: an
// interleaved run keeps its order, and the reserved element is replaced in
// place rather than left behind as an empty text element.
func TestChatVideoKeepsPartOrder(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		_ *http.Request,
		_ map[string]any,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, chatResponseJSON)
	})
	defer server.Close()

	source, err := media.NewVideoURL("https://cdn.example.com/clip.mp4", "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoURL: %v", err)
	}
	request := simpleTextRequest("before")
	request.Input.Content.Parts = []message.Part{
		message.TextPart{Text: "before"},
		message.VideoPart{Source: source},
		message.TextPart{Text: "after"},
	}
	compiled, err := compileChat("gpt-5.6-sol", videoEntry())(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileChat: %v", err)
	}
	if _, err := transportChatGenerate(testClients(t, server).api)(
		context.Background(), compiled.Wire,
	); err != nil {
		t.Fatalf("transport: %v", err)
	}

	content := chatContentArray(t, capture.body(0))
	if len(content) != 3 {
		t.Fatalf("content = %#v, want text, video, text", content)
	}
	first := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "before" {
		t.Fatalf("content[0] = %#v, want the leading text", first)
	}
	video := content[1].(map[string]any)
	if video["type"] != "video_url" {
		t.Fatalf("content[1] = %#v, want the video element", video)
	}
	last := content[2].(map[string]any)
	if last["type"] != "text" || last["text"] != "after" {
		t.Fatalf("content[2] = %#v, want the trailing text", last)
	}
}

// testVideoStream stands in for a live source: the lowering must reject it
// before the transport ever sees it, so it never has to produce a frame.
type testVideoStream struct{}

func (testVideoStream) Read(context.Context) (message.Part, error) {
	return message.TextPart{Text: "frame"}, nil
}
