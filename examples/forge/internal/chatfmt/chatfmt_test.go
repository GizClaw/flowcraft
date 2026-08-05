package chatfmt

import (
	"strings"
	"testing"
)

func TestCollectorSplitsByNodeID(t *testing.T) {
	var c Collector
	c.Token("host_opening", "天黑请闭眼。")
	c.Token("host_opening", "狼人请睁眼。")
	c.Token("seat_1_lin_zhi_speak", "我1号林知先发言。")
	c.Token("", "（无节点标识，并入当前块）")

	if got := c.Text(); got != "天黑请闭眼。狼人请睁眼。我1号林知先发言。（无节点标识，并入当前块）" {
		t.Fatalf("Text = %q", got)
	}
	if len(c.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(c.Blocks))
	}
	labels := map[string]string{
		"host_opening":        "主持人",
		"seat_1_lin_zhi_speak": "1号林知",
	}
	got := Render(c.Blocks, func(nodeID string) string { return labels[nodeID] })
	want := "[主持人] 天黑请闭眼。狼人请睁眼。\n[1号林知] 我1号林知先发言。（无节点标识，并入当前块）"
	if got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}
}

func TestCollectorToolBlocks(t *testing.T) {
	var c Collector
	c.ToolCall("werewolf_game_event", `{"event_type":"night_resolve"}`)
	c.Token("host_day_announce", "现在是第1天白天。")
	c.ToolResult("werewolf_game_event", `{"ok":true,"simulated":true}`)

	if got := c.Text(); got != "现在是第1天白天。" {
		t.Fatalf("Text = %q, want speech only", got)
	}
	got := Render(c.Blocks, func(string) string { return "" })
	want := "[工具调用] werewolf_game_event {\"event_type\":\"night_resolve\"}\n现在是第1天白天。\n[工具结果] werewolf_game_event: {\"ok\":true,\"simulated\":true}"
	if got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}
}

func TestRenderNeverLeaksNodeID(t *testing.T) {
	var c Collector
	c.Token("some_internal_node", "内部文本")
	got := Render(c.Blocks, func(string) string { return "" })
	if got != "内部文本" {
		t.Fatalf("Render = %q, want bare text", got)
	}
	if strings.Contains(got, "some_internal_node") {
		t.Fatal("Render must not leak the raw node id")
	}
}
