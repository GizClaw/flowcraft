package bytedance

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestDecodeGenerateStampsReasoningScope pins the decode half for this
// driver: ark emits reasoning traces, and every one leaves the decoder
// stamped with the verification scope of the model that produced it, so a
// conversation that later moves to a target which replays traces can tell
// where the trace came from instead of treating it as its own.
func TestDecodeGenerateStampsReasoningScope(t *testing.T) {
	scope := inference.ReasoningScope(
		"", "doubao", "doubao-seed-2-0-lite", "default")
	decode := inference.WithReasoningSource(decodeGenerate, scope)
	response, err := decode(context.Background(), generateRaw{
		id:     "resp_1",
		finish: inference.FinishCompleted,
		reasonings: []rawReasoning{
			{id: "rs_1", text: "thinking"},
		},
		texts: []string{"answer"},
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	parts := response.Message.Content.Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %+v", parts)
	}
	reasoning, ok := parts[0].(message.ReasoningPart)
	if !ok || reasoning.Text != "thinking" || reasoning.ID != "rs_1" {
		t.Fatalf("reasoning part = %#v", parts[0])
	}
	if reasoning.Source != scope {
		t.Fatalf("reasoning source = %q, want %q", reasoning.Source, scope)
	}
}

// TestReasoningScopeSpecValidation pins the declared token's shape rules.
func TestReasoningScopeSpecValidation(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		wantErr bool
	}{
		{raw: `{}`},
		{raw: `{"reasoning_scope":"prod-eu"}`},
		{raw: `{"reasoning_scope":" prod"}`, wantErr: true},
		{raw: `{"reasoning_scope":"prod\neu"}`, wantErr: true},
		{raw: `{"reasoning_scope":"` + strings.Repeat("x", 129) + `"}`, wantErr: true},
	} {
		_, err := decodeSpec(context.Background(), []byte(tc.raw))
		if tc.wantErr && err == nil {
			t.Errorf("decodeSpec(%s) succeeded, want an error", tc.raw)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("decodeSpec(%s): %v", tc.raw, err)
		}
	}
}
