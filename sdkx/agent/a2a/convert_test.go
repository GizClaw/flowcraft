package a2a

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
	a2aprotocol "github.com/a2aproject/a2a-go/v2/a2a"
)

func TestA2APartFromMessagePart(t *testing.T) {
	tests := []struct {
		name     string
		part     message.Part
		wantText string
		wantURL  string
		wantData string
		handled  bool
	}{
		{name: "text", part: message.TextPart{Text: "hi"}, wantText: "hi", handled: true},
		{name: "file", part: message.FilePart{URI: "https://x/y.png", MediaType: "image/png", Name: "y.png"},
			wantURL: "https://x/y.png", handled: true},
		{name: "data", part: message.DataPart{Value: json.RawMessage(`{"k":"v"}`)},
			wantData: `{"k":"v"}`, handled: true},
		{name: "tool-call-skipped", part: message.ToolCallPart{Call: message.Call{ID: "c", Name: "f"}}, handled: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, handled, err := a2aPartFromMessagePart(tt.part)
			if err != nil {
				t.Fatalf("a2aPartFromMessagePart: %v", err)
			}
			if handled != tt.handled {
				t.Fatalf("handled = %v, want %v", handled, tt.handled)
			}
			if !handled {
				return
			}
			if tt.wantText != "" && got.Text() != tt.wantText {
				t.Errorf("text = %q, want %q", got.Text(), tt.wantText)
			}
			if tt.wantURL != "" && string(got.URL()) != tt.wantURL {
				t.Errorf("url = %q, want %q", got.URL(), tt.wantURL)
			}
			if tt.wantData != "" {
				raw, _ := json.Marshal(got.Data())
				if string(raw) != tt.wantData {
					t.Errorf("data = %s, want %s", raw, tt.wantData)
				}
			}
		})
	}
}

func TestA2APartFromMediaSource(t *testing.T) {
	src, err := media.NewImageURL("https://x/a.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	part, handled, err := a2aPartFromMediaSource(src, "a.png")
	if err != nil || !handled {
		t.Fatalf("a2aPartFromMediaSource: handled=%v err=%v", handled, err)
	}
	if string(part.URL()) != "https://x/a.png" || part.MediaType != "image/png" {
		t.Errorf("url part = %+v", part)
	}

	bytesSrc, err := media.NewImageBytes([]byte{1, 2, 3}, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	part, handled, err = a2aPartFromMediaSource(bytesSrc, "")
	if err != nil || !handled {
		t.Fatalf("a2aPartFromMediaSource bytes: handled=%v err=%v", handled, err)
	}
	if len(part.Raw()) != 3 || part.MediaType != "image/png" {
		t.Errorf("raw part = %+v", part)
	}
}

func TestMessagePartsFromA2A(t *testing.T) {
	parts := []*a2aprotocol.Part{
		a2aprotocol.NewTextPart("text"),
		a2aprotocol.NewFileURLPart("https://x/y.txt", "text/plain"),
		a2aprotocol.NewRawPart([]byte("raw-bytes")),
		a2aprotocol.NewDataPart(map[string]any{"k": "v"}),
	}
	out := messagePartsFromA2A(parts)
	if len(out) != 4 {
		t.Fatalf("converted parts = %d, want 4", len(out))
	}
	if tp, ok := out[0].(message.TextPart); !ok || tp.Text != "text" {
		t.Errorf("part[0] = %#v, want TextPart text", out[0])
	}
	if fp, ok := out[1].(message.FilePart); !ok || fp.URI != "https://x/y.txt" {
		t.Errorf("part[1] = %#v, want FilePart url", out[1])
	}
	// Raw bytes ride as a data: URI FilePart.
	fp, ok := out[2].(message.FilePart)
	if !ok || fp.URI == "" || len(fp.URI) <= len("data:") {
		t.Errorf("part[2] = %#v, want FilePart with data: URI", out[2])
	}
	if dp, ok := out[3].(message.DataPart); !ok || string(dp.Value) != `{"k":"v"}` {
		t.Errorf("part[3] = %#v, want DataPart", out[3])
	}
}

func TestMessageText(t *testing.T) {
	m := &a2aprotocol.Message{Parts: []*a2aprotocol.Part{
		a2aprotocol.NewTextPart("line one"),
		a2aprotocol.NewTextPart("line two"),
	}}
	if got := messageText(m); got != "line one\nline two" {
		t.Errorf("messageText = %q", got)
	}
	if got := messageText(nil); got != "" {
		t.Errorf("messageText(nil) = %q, want empty", got)
	}
}
