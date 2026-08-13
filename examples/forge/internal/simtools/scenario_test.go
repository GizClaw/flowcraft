package simtools_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"

	"github.com/GizClaw/flowcraft/examples/forge/internal/simtools"
)

func TestSimToolsSourceExposesExpectedTools(t *testing.T) {
	value, err := simtools.NewSourceFactory(new(atomic.Int64)).New(
		context.Background(),
		resource.Input{Settings: []byte(`{}`)},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	src, ok := value.(tool.Source)
	if !ok {
		t.Fatalf("New returned %T, want tool.Source", value)
	}
	names := make(map[string]bool)
	for _, definition := range src.Tools() {
		names[definition.Definition().Name] = true
	}
	for _, want := range []string{
		"play_music",
		"set_device_volume",
		"stop_playback",
		"werewolf_game_event",
	} {
		if !names[want] {
			t.Errorf("source is missing tool %q", want)
		}
	}
	if len(src.LazyTools()) != 0 {
		t.Errorf("sim source unexpectedly exposes lazy tools")
	}
}
