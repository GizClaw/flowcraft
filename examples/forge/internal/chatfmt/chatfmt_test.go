package chatfmt

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

func call(id, name, arguments string) agent.ToolCallStart {
	return agent.ToolCallStart{Call: message.ToolCall{
		ID:        id,
		Name:      name,
		Arguments: message.NewArguments([]byte(arguments)),
	}}
}

func result(id, text string) agent.ToolResult {
	return agent.ToolResult{Result: message.ToolResult{CallID: id, Content: message.NewTextContent(text)}}
}

func TestCollectorBuildsSpeechAndToolBlocks(t *testing.T) {
	collector := NewCollector()
	for _, event := range []agent.Event{
		agent.StreamStart{Round: 1},
		agent.Text{Text: "好，"},
		agent.Text{Text: "正在播放。"},
		call("call-1", "play_music", `{"title":"海阔天空"}`),
		result("call-1", `{"ok":true}`),
		agent.Text{Text: "音量已调小。"},
		agent.UsageEvent{Usage: usage.Usage{InputTokens: 10, OutputTokens: 4}},
		agent.Status{State: agent.StateCompleted},
	} {
		collector.Observe(event)
	}

	blocks := collector.Blocks()
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want 4", len(blocks))
	}
	want := []BlockKind{Speech, ToolCall, ToolResult, Speech}
	for i, kind := range want {
		if blocks[i].Kind != kind {
			t.Fatalf("block %d kind = %q, want %q", i, blocks[i].Kind, kind)
		}
	}
	if got := blocks[1].Name; got != "play_music" {
		t.Fatalf("tool block name = %q", got)
	}
	if got := blocks[2].Name; got != "play_music" {
		t.Fatalf("tool result name = %q, want the call name resolved by id", got)
	}
	if got := collector.Tokens(); got != 3 {
		t.Fatalf("tokens = %d, want 3", got)
	}
	// Text is what a simulation feeds back: speech only, no tool noise.
	if got := collector.Text(); got != "好，正在播放。音量已调小。" {
		t.Fatalf("text = %q", got)
	}
}

func TestCollectorSplitsSpeechAroundAToolCall(t *testing.T) {
	collector := NewCollector()
	collector.Observe(agent.Text{Text: "先找一下。"})
	collector.Observe(call("c1", "search", `{}`))
	collector.Observe(result("c1", "found"))
	collector.Observe(agent.Text{Text: "找到了。"})

	blocks := collector.Blocks()
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want speech/call/result/speech", len(blocks))
	}
	if blocks[0].Text.String() != "先找一下。" || blocks[3].Text.String() != "找到了。" {
		t.Fatalf("speech blocks = %q / %q", blocks[0].Text.String(), blocks[3].Text.String())
	}
}

func TestCollectorResolvesAnUnknownCallIDToItself(t *testing.T) {
	collector := NewCollector()
	collector.Observe(result("orphan", "no name known"))
	got := Render(collector.Blocks())
	if !strings.Contains(got, "[工具结果] orphan: no name known") {
		t.Fatalf("render = %q", got)
	}
}

func TestRenderLabelsToolBlocks(t *testing.T) {
	collector := NewCollector()
	collector.Observe(agent.Text{Text: "好的。"})
	collector.Observe(call("c1", "set_device_volume", `{"pct":30}`))
	collector.Observe(result("c1", `{"ok":true}`))
	got := Render(collector.Blocks())
	for _, want := range []string{"好的。", "[工具调用] set_device_volume {\"pct\":30}", "[工具结果] set_device_volume: {\"ok\":true}"} {
		if !strings.Contains(got, want) {
			t.Fatalf("render %q missing %q", got, want)
		}
	}
}

func TestResultTextReportsOnlyAFinalAnswer(t *testing.T) {
	answer := agent.Result{Content: message.NewTextContent("  完成  ")}
	if got := ResultText(answer); got != "完成" {
		t.Fatalf("ResultText = %q", got)
	}
	if got := ResultText(agent.Result{}); got != "" {
		t.Fatalf("empty result = %q, want no text", got)
	}
}

// TestPrivateLinesAreBadgedOnce pins the surface half of the privacy split: the
// collector labels a line with the phase it was told, and rendering says so without
// knowing what any phase means.
func TestPrivateLinesAreBadgedOnce(t *testing.T) {
	c := NewCollector()
	c.SetSpeaker("1号林知")
	c.SetPhase("night_wolf", true)
	c.Observe(agent.Text{Text: "今晚刀 3 号。"})
	c.SetSpeaker("4号小满")
	c.SetPhase("day", false)
	c.Observe(agent.Text{Text: "我觉得 3 号有问题。"})

	out := Render(c.Blocks())
	if strings.Count(out, "[私密]") != 1 {
		t.Fatalf("badges = %d\n%s", strings.Count(out, "[私密]"), out)
	}
	if !strings.Contains(out, "[私密] [1号林知] 今晚刀 3 号。") {
		t.Fatalf("private line lost its speaker or badge:\n%s", out)
	}
	if strings.Contains(out, "[私密] [4号小满]") {
		t.Fatalf("a public line was badged:\n%s", out)
	}
	if c.Blocks()[0].Phase != "night_wolf" || c.Blocks()[1].Phase != "day" {
		t.Fatalf("phases = %q %q", c.Blocks()[0].Phase, c.Blocks()[1].Phase)
	}
}
