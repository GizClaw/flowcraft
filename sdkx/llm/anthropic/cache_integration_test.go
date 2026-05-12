package anthropic

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// captureBody is an httptest.Handler that records the most recent
// request body so a test can assert against the actual wire format the
// adapter emits, end-to-end through the live anthropic-sdk-go client.
// Each test stands up its own server; the captured body is checked
// against expectations after a single Generate call.
type captureBody struct {
	mu  *capturedRequest
	rsp string
}

type capturedRequest struct {
	body []byte
}

func (h captureBody) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	h.mu.body = b
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	rsp := h.rsp
	if rsp == "" {
		rsp = `{
			"id": "msg_test",
			"type": "message",
			"role": "assistant",
			"model": "claude-test",
			"content": [{"type": "text", "text": "ok"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 1, "output_tokens": 1}
		}`
	}
	_, _ = io.WriteString(w, rsp)
}

func newCaptureServer(t *testing.T) (*httptest.Server, *capturedRequest) {
	t.Helper()
	cap := &capturedRequest{}
	ts := httptest.NewServer(captureBody{mu: cap})
	t.Cleanup(ts.Close)
	return ts, cap
}

// requestBody is the just-enough shape of the Anthropic Messages API
// request body that the cache-anchor tests need to introspect. The
// SDK Marshal output is JSON-compatible with the public API shape, so
// we decode straight into this; unknown fields are ignored.
type requestBody struct {
	System []struct {
		Type         string                 `json:"type"`
		Text         string                 `json:"text"`
		CacheControl map[string]interface{} `json:"cache_control,omitempty"`
	} `json:"system"`
	Messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Type         string                 `json:"type"`
			Text         string                 `json:"text"`
			CacheControl map[string]interface{} `json:"cache_control,omitempty"`
		} `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name         string                 `json:"name"`
		CacheControl map[string]interface{} `json:"cache_control,omitempty"`
	} `json:"tools,omitempty"`
}

func decodeBody(t *testing.T, raw []byte) requestBody {
	t.Helper()
	var rb requestBody
	if err := json.Unmarshal(raw, &rb); err != nil {
		t.Fatalf("decode request body: %v\nbody: %s", err, raw)
	}
	return rb
}

// TestGenerate_MultiSystemSegments_BecomesMultiBlock locks in the core
// contract change: multiple llm.Message{Role:System} entries land as
// independent text blocks on the wire instead of being joined with
// "\n". This is the primitive that the cache-anchor heuristic relies
// on, so the assertion is critical regardless of cache logic.
func TestGenerate_MultiSystemSegments_BecomesMultiBlock(t *testing.T) {
	ts, cap := newCaptureServer(t)
	c, _ := New("claude-test", "k", ts.URL, nil)

	_, _, err := c.Generate(t.Context(),
		[]llm.Message{
			llm.NewTextMessage(llm.RoleSystem, "segment-A"),
			llm.NewTextMessage(llm.RoleSystem, "segment-B"),
			llm.NewTextMessage(llm.RoleSystem, "segment-C"),
			llm.NewTextMessage(llm.RoleUser, "hi"),
		},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	rb := decodeBody(t, cap.body)
	if len(rb.System) != 3 {
		t.Fatalf("expected 3 system blocks, got %d (body=%s)", len(rb.System), cap.body)
	}
	wantTexts := []string{"segment-A", "segment-B", "segment-C"}
	for i, w := range wantTexts {
		if rb.System[i].Text != w {
			t.Errorf("system[%d].Text = %q, want %q", i, rb.System[i].Text, w)
		}
	}
	// All segments here are short, so none should have cache_control.
	for i, blk := range rb.System {
		if blk.CacheControl != nil {
			t.Errorf("system[%d] unexpectedly has cache_control: %+v", i, blk.CacheControl)
		}
	}
}

// TestGenerate_LongStableSegment_GetsCacheControl exercises the
// happy path: a long stable segment followed by a short volatile one,
// only the long stable segment gets cache_control on the wire.
func TestGenerate_LongStableSegment_GetsCacheControl(t *testing.T) {
	ts, cap := newCaptureServer(t)
	c, _ := New("claude-test", "k", ts.URL, nil)

	stable := strings.Repeat("S", anthropicCacheMinChars+1)
	volatile := strings.Repeat("v", 100)

	_, _, err := c.Generate(t.Context(),
		[]llm.Message{
			llm.NewTextMessage(llm.RoleSystem, stable),
			llm.NewTextMessage(llm.RoleSystem, volatile),
			llm.NewTextMessage(llm.RoleUser, "hi"),
		},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	rb := decodeBody(t, cap.body)
	if len(rb.System) != 2 {
		t.Fatalf("expected 2 system blocks, got %d", len(rb.System))
	}
	if rb.System[0].CacheControl == nil {
		t.Errorf("stable segment should have cache_control, got nil")
	} else if rb.System[0].CacheControl["type"] != "ephemeral" {
		t.Errorf("stable segment cache_control type = %v, want ephemeral", rb.System[0].CacheControl["type"])
	}
	if rb.System[1].CacheControl != nil {
		t.Errorf("volatile segment should NOT have cache_control, got %+v", rb.System[1].CacheControl)
	}
}

// TestGenerate_FiveLongSegments_BudgetCapAt4 verifies budget-trimming:
// with 5 cache-eligible segments and only 4 breakpoint slots, the
// earliest anchor is dropped (keeping the latest 4 = longest fallback
// chain).
func TestGenerate_FiveLongSegments_BudgetCapAt4(t *testing.T) {
	ts, cap := newCaptureServer(t)
	c, _ := New("claude-test", "k", ts.URL, nil)

	mk := func(marker rune) llm.Message {
		return llm.NewTextMessage(llm.RoleSystem, strings.Repeat(string(marker), anthropicCacheMinChars+1))
	}
	_, _, err := c.Generate(t.Context(),
		[]llm.Message{
			mk('A'), mk('B'), mk('C'), mk('D'), mk('E'),
			llm.NewTextMessage(llm.RoleUser, "hi"),
		},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	rb := decodeBody(t, cap.body)
	if len(rb.System) != 5 {
		t.Fatalf("expected 5 system blocks, got %d", len(rb.System))
	}
	// Earliest segment (index 0, 'A') is dropped from the anchor set
	// to fit the 4-breakpoint budget; indices 1–4 all carry the
	// marker.
	if rb.System[0].CacheControl != nil {
		t.Errorf("system[0] should NOT have cache_control (dropped for budget)")
	}
	for i := 1; i < 5; i++ {
		if rb.System[i].CacheControl == nil {
			t.Errorf("system[%d] should have cache_control", i)
		}
	}
}

// TestGenerate_NoCacheControlWhenAllShort confirms the conservative
// default: when no segment qualifies, the wire request carries zero
// cache_control fields. Avoids the 25%-write / 0%-read failure mode
// of anchoring sub-threshold prefixes.
func TestGenerate_NoCacheControlWhenAllShort(t *testing.T) {
	ts, cap := newCaptureServer(t)
	c, _ := New("claude-test", "k", ts.URL, nil)

	_, _, err := c.Generate(t.Context(),
		[]llm.Message{
			llm.NewTextMessage(llm.RoleSystem, "short A"),
			llm.NewTextMessage(llm.RoleSystem, "short B"),
			llm.NewTextMessage(llm.RoleUser, "hi"),
		},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	rb := decodeBody(t, cap.body)
	for i, blk := range rb.System {
		if blk.CacheControl != nil {
			t.Errorf("system[%d] should not have cache_control, got %+v", i, blk.CacheControl)
		}
	}
	for i, m := range rb.Messages {
		for j, blk := range m.Content {
			if blk.CacheControl != nil {
				t.Errorf("msg[%d].content[%d] should not have cache_control, got %+v", i, j, blk.CacheControl)
			}
		}
	}
}

// TestGenerate_HistoryAnchorForMultiTurn verifies the history anchor
// fires on a multi-turn conversation with long prior turns. The
// second-to-last message's final block carries cache_control;
// earlier turns do not get individual markers.
func TestGenerate_HistoryAnchorForMultiTurn(t *testing.T) {
	ts, cap := newCaptureServer(t)
	c, _ := New("claude-test", "k", ts.URL, nil)

	longTurn := func(c rune) string { return strings.Repeat(string(c), anthropicCacheMinChars+1) }
	// Use alternating user/assistant turns so mergeOrAppend doesn't
	// collapse consecutive same-role messages below the
	// anthropicMinMessagesForHistoryCache (4) gate.
	_, _, err := c.Generate(t.Context(),
		[]llm.Message{
			llm.NewTextMessage(llm.RoleUser, longTurn('U')),
			llm.NewTextMessage(llm.RoleAssistant, longTurn('A')),
			llm.NewTextMessage(llm.RoleUser, longTurn('V')),
			llm.NewTextMessage(llm.RoleAssistant, longTurn('B')),
			llm.NewTextMessage(llm.RoleUser, "fresh query"),
		},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	rb := decodeBody(t, cap.body)
	if len(rb.Messages) < 4 {
		t.Fatalf("expected ≥4 messages after conversion, got %d", len(rb.Messages))
	}
	// historyMsgIdx targets the second-to-last MessageParam (here
	// msgs[3] — the assistant's "B" turn), so the final block of
	// that message must carry cache_control. The final user turn
	// ("fresh query") at msgs[4] must remain unmarked.
	target := len(rb.Messages) - 2
	content := rb.Messages[target].Content
	if len(content) == 0 {
		t.Fatalf("target message has no content")
	}
	last := content[len(content)-1]
	if last.CacheControl == nil {
		t.Errorf("expected cache_control on last block of msg[%d], got nil; body=%s", target, cap.body)
	}
	// Final message (the "fresh query") must not be anchored.
	finalMsg := rb.Messages[len(rb.Messages)-1]
	for j, blk := range finalMsg.Content {
		if blk.CacheControl != nil {
			t.Errorf("final msg block[%d] should not be anchored, got %+v", j, blk.CacheControl)
		}
	}
}
