package tool

import (
	"context"
	"testing"
)

type metaTool struct {
	def  Definition
	meta ToolMeta
}

func (m *metaTool) Definition() Definition                              { return m.def }
func (m *metaTool) Execute(_ context.Context, _ string) (string, error) { return "ok", nil }
func (m *metaTool) Metadata() ToolMeta                                  { return m.meta }

func TestMetadataOf_DefaultZero(t *testing.T) {
	if got := MetadataOf(nil); got != (ToolMeta{}) {
		t.Errorf("MetadataOf(nil) = %+v, want zero", got)
	}
	if got := MetadataOf(stubTool("plain")); got != (ToolMeta{}) {
		t.Errorf("MetadataOf(plain) = %+v, want zero", got)
	}
}

func TestMetadataOf_DeclaredValues(t *testing.T) {
	tl := &metaTool{
		def:  Definition{Name: "writer"},
		meta: ToolMeta{RateLimit: 5, MutatesState: true},
	}
	got := MetadataOf(tl)
	if got.RateLimit != 5 {
		t.Errorf("RateLimit = %v, want 5", got.RateLimit)
	}
	if !got.MutatesState {
		t.Errorf("MutatesState = false, want true")
	}
}
