package script

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestPlainValueProjectsEveryReadableKind(t *testing.T) {
	history := graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
		message.NewTextMessage(message.RoleUser, "你好"),
		message.NewTextMessage(message.RoleAssistant, "在"),
	}))
	cases := map[string]struct {
		value graph.Value
		check func(t *testing.T, plain any)
	}{
		"text": {graph.Text("hi"), func(t *testing.T, plain any) {
			if plain != "hi" {
				t.Fatalf("plain = %v", plain)
			}
		}},
		"bool": {graph.Bool(true), func(t *testing.T, plain any) {
			if plain != true {
				t.Fatalf("plain = %v", plain)
			}
		}},
		"json": {graph.JSON([]byte(`{"n":1}`)), func(t *testing.T, plain any) {
			raw, ok := plain.(json.RawMessage)
			if !ok || string(raw) != `{"n":1}` {
				t.Fatalf("plain = %T %v", plain, plain)
			}
		}},
		"messages": {history, func(t *testing.T, plain any) {
			projected, ok := plain.(Messages)
			if !ok {
				t.Fatalf("plain = %T, want the transcript projection", plain)
			}
			if projected.Len() != 2 {
				t.Fatalf("len = %d", projected.Len())
			}
			first := projected.Slice()[0]
			if first.Role != message.RoleUser || first.Content.Text() != "你好" {
				t.Fatalf("first = %v", first)
			}
		}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			plain, err := plainValue(test.value)
			if err != nil {
				t.Fatalf("plainValue: %v", err)
			}
			test.check(t, plain)
		})
	}
}

func TestPlainValueRefusesMedia(t *testing.T) {
	if _, err := plainValue(graph.Image("image/png", []byte("x"))); err == nil {
		t.Fatal("an image has no projection into a rule")
	} else if !strings.Contains(err.Error(), "cannot be projected") {
		t.Fatalf("error = %v", err)
	}
}

func TestStateValueConvertsEveryWritableKind(t *testing.T) {
	cases := map[string]struct {
		kind  graph.Kind
		value any
		check func(t *testing.T, result graph.Value, size int)
	}{
		"text": {graph.KindText, "hello", func(t *testing.T, result graph.Value, size int) {
			if got, _ := result.Text(); got != "hello" || size != 5 {
				t.Fatalf("value = %q size = %d", got, size)
			}
		}},
		"bool": {graph.KindBool, true, func(t *testing.T, result graph.Value, size int) {
			if got, _ := result.Bool(); !got || size != 1 {
				t.Fatalf("value = %v size = %d", got, size)
			}
		}},
		"json": {graph.KindJSON, map[string]any{"n": 1}, func(t *testing.T, result graph.Value, size int) {
			raw, _ := result.JSON()
			if string(raw) != `{"n":1}` || size != len(raw) {
				t.Fatalf("value = %s size = %d", raw, size)
			}
		}},
		"messages": {graph.KindMessages, []any{
			map[string]any{"role": "user", "text": "你好"},
			map[string]any{"role": "assistant", "text": "在"},
		}, func(t *testing.T, result graph.Value, size int) {
			history, err := result.Messages()
			if err != nil {
				t.Fatalf("messages: %v", err)
			}
			if history.Len() != 2 {
				t.Fatalf("messages = %d", history.Len())
			}
			if size == 0 {
				t.Fatal("size should count the role and text bytes")
			}
		}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			result, size, err := stateValue(test.kind, test.value)
			if err != nil {
				t.Fatalf("stateValue: %v", err)
			}
			test.check(t, result, size)
		})
	}
}

func TestStateValueRefusesWhatItCannotWrite(t *testing.T) {
	cases := map[string]struct {
		kind  graph.Kind
		value any
		want  string
	}{
		"text for a bool key":           {graph.KindBool, "x", "want a bool"},
		"bool for a text key":           {graph.KindText, true, "want a string"},
		"unencodable json":              {graph.KindJSON, make(chan int), "encode as JSON"},
		"messages that are not a list":  {graph.KindMessages, "x", "want a list"},
		"message that is not an object": {graph.KindMessages, []any{"x"}, "want an object"},
		"message without a role":        {graph.KindMessages, []any{map[string]any{"text": "x"}}, "role is required"},
		"unknown role":                  {graph.KindMessages, []any{map[string]any{"role": "ghost", "text": "x"}}, "unknown message role"},
		"media":                         {graph.KindImage, "x", "cannot be written"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := stateValue(test.kind, test.value)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v does not mention %q", err, test.want)
			}
		})
	}
}

func TestPayloadFallsBackWhenAValueCannotEncode(t *testing.T) {
	if got := string(payload(make(chan int))); got != "{}" {
		t.Fatalf("payload = %s, want an empty object", got)
	}
}

func TestLogLevelNames(t *testing.T) {
	for level, want := range map[LogLevel]string{
		LogDebug:    "debug",
		LogInfo:     "info",
		LogWarn:     "warn",
		LogLevel(9): "unknown",
	} {
		if got := level.String(); got != want {
			t.Fatalf("%d = %q, want %q", level, got, want)
		}
	}
}
