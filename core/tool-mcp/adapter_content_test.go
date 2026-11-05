package mcp

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestResultContentMapsTypedBlocks(t *testing.T) {
	res := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
		&mcpsdk.TextContent{Text: "hello"},
		&mcpsdk.ImageContent{Data: []byte{1, 2, 3}, MIMEType: "image/png"},
		&mcpsdk.AudioContent{Data: []byte{4, 5}, MIMEType: "audio/mpeg"},
		&mcpsdk.ResourceLink{URI: "file:///tmp/a.txt", MIMEType: "text/plain", Name: "a.txt"},
	}}
	content := resultContent(res)
	if len(content.Parts) != 4 {
		t.Fatalf("parts = %d, want 4", len(content.Parts))
	}
	if text, ok := content.Parts[0].(message.TextPart); !ok || text.Text != "hello" {
		t.Fatalf("part 0 = %#v, want text part", content.Parts[0])
	}
	image, ok := content.Parts[1].(message.ImagePart)
	if !ok || image.Source.MediaType() != "image/png" {
		t.Fatalf("part 1 = %#v, want image/png part", content.Parts[1])
	}
	if audio, ok := content.Parts[2].(message.AudioPart); !ok || audio.Source.MediaType() != "audio/mpeg" {
		t.Fatalf("part 2 = %#v, want audio/mpeg part", content.Parts[2])
	}
	file, ok := content.Parts[3].(message.FilePart)
	if !ok || file.URI != "file:///tmp/a.txt" || file.Name != "a.txt" {
		t.Fatalf("part 3 = %#v, want file part", content.Parts[3])
	}
	if err := content.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestResultContentMapsEmbeddedResources(t *testing.T) {
	res := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
		&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{
			URI: "file:///tmp/notes.md", MIMEType: "text/markdown", Text: "notes",
		}},
		&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{
			URI: "file:///tmp/diagram.png", MIMEType: "image/png", Blob: []byte{9, 9},
		}},
	}}
	content := resultContent(res)
	if len(content.Parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(content.Parts))
	}
	if text, ok := content.Parts[0].(message.TextPart); !ok || text.Text != "notes" {
		t.Fatalf("part 0 = %#v, want text part", content.Parts[0])
	}
	if _, ok := content.Parts[1].(message.ImagePart); !ok {
		t.Fatalf("part 1 = %T, want image part", content.Parts[1])
	}
}

func TestResultContentKeepsUnmappedBlocksAsData(t *testing.T) {
	// A block the local model has no typed home for must survive as JSON
	// rather than being dropped.
	res := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
		// A resource link without a URI cannot become a file part.
		&mcpsdk.ResourceLink{Name: "broken"},
		// An image without a usable media type cannot become an image
		// part, so it falls back to its wire form (base64 payload included).
		&mcpsdk.ImageContent{Data: []byte{1}, MIMEType: ""},
	}}
	content := resultContent(res)
	if len(content.Parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(content.Parts))
	}
	data, ok := content.Parts[0].(message.DataPart)
	if !ok {
		t.Fatalf("part 0 = %T, want data part", content.Parts[0])
	}
	if data.MediaType != "application/json" || len(data.Value) == 0 {
		t.Fatalf("data part = %#v", data)
	}
	if _, ok := content.Parts[1].(message.DataPart); !ok {
		t.Fatalf("part 1 = %T, want data part", content.Parts[1])
	}
}

func TestResultContentCarriesStructuredContent(t *testing.T) {
	content := resultContent(&mcpsdk.CallToolResult{
		StructuredContent: map[string]any{"answer": 42},
	})
	if len(content.Parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(content.Parts))
	}
	data, ok := content.Parts[0].(message.DataPart)
	if !ok || data.MediaType != "application/json" {
		t.Fatalf("part = %#v, want JSON data part", content.Parts[0])
	}
}

func TestResultContentHandlesEmpty(t *testing.T) {
	for name, res := range map[string]*mcpsdk.CallToolResult{
		"nil":        nil,
		"empty":      {},
		"empty text": {Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: ""}}},
	} {
		t.Run(name, func(t *testing.T) {
			content := resultContent(res)
			if len(content.Parts) != 1 {
				t.Fatalf("parts = %d, want 1 (content must stay valid)", len(content.Parts))
			}
			if got := content.Text(); got != "" {
				t.Fatalf("Render = %q, want empty", got)
			}
			if err := content.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestResultContentEmptyTextFallsBackToStructuredContent(t *testing.T) {
	content := resultContent(&mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: ""}},
		StructuredContent: map[string]any{"answer": 42},
	})
	if len(content.Parts) != 1 {
		t.Fatalf("parts = %d, want the structured content", len(content.Parts))
	}
	data, ok := content.Parts[0].(message.DataPart)
	if !ok || data.MediaType != "application/json" {
		t.Fatalf("part = %#v, want a JSON data part", content.Parts[0])
	}
	if !strings.Contains(string(data.Value), `"answer":42`) {
		t.Fatalf("data value = %s, want the structured content", data.Value)
	}
}
