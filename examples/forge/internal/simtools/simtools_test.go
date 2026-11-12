package simtools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/tool"
)

func TestSourceToolsAreValid(t *testing.T) {
	source := New()
	if len(source.Tools()) == 0 {
		t.Fatal("source contributes no tools")
	}
	for _, item := range source.Tools() {
		if err := item.Definition().Validate(); err != nil {
			t.Fatalf("definition: %v", err)
		}
		var schema map[string]any
		if err := json.Unmarshal(item.Definition().InputSchema, &schema); err != nil {
			t.Fatalf("%s schema: %v", item.Definition().Name, err)
		}
	}
	var _ tool.Source = source
}

func TestExecuteCountsAndEchoesArguments(t *testing.T) {
	source := New()
	item, ok := lookup(source, "play_music")
	if !ok {
		t.Fatal("play_music is not contributed")
	}
	content, err := item.Execute(context.Background(), `{"title":"海阔天空"}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if source.Calls() != 1 {
		t.Fatalf("calls = %d, want 1", source.Calls())
	}
	text := content.Text()
	for _, want := range []string{`"tool":"play_music"`, `"title":"海阔天空"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("result %s missing %s", text, want)
		}
	}
}

func TestExecuteToleratesMalformedArguments(t *testing.T) {
	source := New()
	item, ok := lookup(source, "stop_playback")
	if !ok {
		t.Fatal("stop_playback is not contributed")
	}
	content, err := item.Execute(context.Background(), "{not json")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(content.Text(), `"args":{}`) {
		t.Fatalf("result = %s, want empty args", content.Text())
	}
}

func lookup(source *Source, name string) (tool.Tool, bool) {
	for _, item := range source.Tools() {
		if item.Definition().Name == name {
			return item, true
		}
	}
	return nil, false
}
