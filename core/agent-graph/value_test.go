package graph

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// Appending must cost one node, not a copy of the session: that is what
// makes a fan-out cost its own work instead of the size of the history.
func TestMessagesAppendSharesThePrefix(t *testing.T) {
	var base Messages
	for i := 0; i < 1000; i++ {
		base = base.Append(message.NewTextMessage(message.RoleUser, "m"))
	}
	if got := base.Len(); got != 1000 {
		t.Fatalf("Len = %d, want 1000", got)
	}
	left := base.Append(message.NewTextMessage(message.RoleUser, "left"))
	right := base.Append(message.NewTextMessage(message.RoleUser, "right"))
	if left.Len() != 1001 || right.Len() != 1001 {
		t.Fatalf("branch lengths = %d/%d, want 1001", left.Len(), right.Len())
	}
	if got := base.Len(); got != 1000 {
		t.Errorf("appending to a branch changed the parent: Len = %d", got)
	}
	leftSlice := left.Slice()
	if got := leftSlice[len(leftSlice)-1].Content.Text(); got != "left" {
		t.Errorf("last message = %q, want left", got)
	}
	if got := right.Slice()[1000].Content.Text(); got != "right" {
		t.Errorf("right branch last message = %q, want right", got)
	}
}

func TestMessagesConcatAppendsTheOther(t *testing.T) {
	a := MessagesFromSlice([]message.Message{message.NewTextMessage(message.RoleUser, "a")})
	b := MessagesFromSlice([]message.Message{message.NewTextMessage(message.RoleUser, "b")})
	got := a.Concat(b)
	if got.Len() != 2 {
		t.Fatalf("Len = %d, want 2", got.Len())
	}
	if text := got.Slice()[1].Content.Text(); text != "b" {
		t.Errorf("second message = %q, want b", text)
	}
}

func TestValueKindIsChecked(t *testing.T) {
	if _, err := JSON([]byte(`{}`)).Text(); err == nil {
		t.Error("Text() on a JSON value = nil, want an error")
	}
	bools, err := Bool(true).Bool()
	if err != nil || !bools {
		t.Errorf("Bool() = %v, %v", bools, err)
	}
}

// A reference carries a blob name and a media type, never the bytes.
func TestImageRefCarriesOnlyABlob(t *testing.T) {
	value := ImageRef("image/png", "blob-7")
	media, err := value.Media()
	if err != nil {
		t.Fatalf("Media: %v", err)
	}
	if media.Blob != "blob-7" || media.MediaType != "image/png" {
		t.Errorf("media = %+v", media)
	}
	if media.Data != nil {
		t.Error("a reference carries inline bytes")
	}
	if value.Kind() != KindImage {
		t.Errorf("kind = %q", value.Kind())
	}
}
