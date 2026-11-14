package wire_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-a2a/wire"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Inline bytes land in the part that matches them; anything else becomes an
// inline file. A nil part is still refused.
func TestFromPartCarriesInlineBytes(t *testing.T) {
	if _, err := wire.FromPart(nil); err == nil {
		t.Error("a nil part was accepted")
	}
	image, err := wire.FromPart(&a2a.Part{Content: a2a.Raw([]byte("png")), MediaType: "image/png"})
	if err != nil {
		t.Fatalf("image raw part: %v", err)
	}
	part, ok := image.(message.ImagePart)
	if !ok || string(part.Source.Bytes()) != "png" {
		t.Fatalf("image part = %#v, want inline bytes", image)
	}
	file, err := wire.FromPart(&a2a.Part{Content: a2a.Raw([]byte("pdf")), MediaType: "application/pdf", Filename: "a.pdf"})
	if err != nil {
		t.Fatalf("file raw part: %v", err)
	}
	filePart, ok := file.(message.FilePart)
	if !ok || string(filePart.Source.Bytes()) != "pdf" || filePart.Name != "a.pdf" {
		t.Fatalf("file part = %#v, want an inline file", file)
	}
}

func TestFromMessageCarriesMetadata(t *testing.T) {
	msg := &a2a.Message{
		ID:   "m1",
		Role: a2a.MessageRoleUser,
		Parts: []*a2a.Part{
			{Content: a2a.Text("hi"), Metadata: map[string]any{"alt": "x"}},
		},
		ReferenceTasks: []a2a.TaskID{"task-1"},
		Extensions:     []string{"https://example.test/ext"},
		Metadata:       map[string]any{"note": map[string]any{"k": float64(1)}},
	}
	core, err := wire.FromMessage(msg)
	if err != nil {
		t.Fatalf("FromMessage: %v", err)
	}
	if core.Metadata == nil {
		t.Fatal("the metadata was dropped")
	}
	if len(core.Metadata.ReferenceTasks) != 1 || core.Metadata.ReferenceTasks[0] != "task-1" {
		t.Errorf("reference tasks = %v", core.Metadata.ReferenceTasks)
	}
	if len(core.Metadata.Extensions) != 1 || core.Metadata.Extensions[0] != "https://example.test/ext" {
		t.Errorf("extensions = %v", core.Metadata.Extensions)
	}
	if got := string(core.Metadata.Attributes["note"]); !strings.Contains(got, `"k":1`) {
		t.Errorf("note = %s, want the protocol metadata", got)
	}
	if got := string(core.Metadata.Attributes[wire.PartAttributeKey(0)]); !strings.Contains(got, `"alt":"x"`) {
		t.Errorf("part metadata = %s, want it kept by position", got)
	}
}

func TestFromMessageCarriesText(t *testing.T) {
	msg, err := wire.FromMessage(a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")))
	if err != nil {
		t.Fatalf("FromMessage: %v", err)
	}
	if msg.Content.Text() != "hello" {
		t.Errorf("content = %q, want hello", msg.Content.Text())
	}
}
