package starlark

import (
	"strings"
	"testing"

	star "go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/message"
)

func view(messages ...message.Message) *messagesValue {
	return newMessages(script.Messages{
		History: graph.MessagesFromSlice(messages),
	}).(*messagesValue)
}

func sampleView() *messagesValue {
	return view(
		message.NewTextMessage(message.RoleUser, "一"),
		message.NewTextMessage(message.RoleAssistant, "二"),
		message.NewTextMessage(message.RoleUser, "三"),
	)
}

// TestMessageInterfaceSurface covers the methods a Starlark value must answer but
// a script only reaches in edge cases: freezing, truth, hashing and a miss.
func TestMessageInterfaceSurface(t *testing.T) {
	value := messageValueOf(message.NewTextMessage(message.RoleUser, "你好"))
	value.Freeze() // a message is already read-only
	if value.Truth() != star.True {
		t.Fatal("a message is truthy")
	}
	if _, err := value.Hash(); err == nil {
		t.Fatal("a message must not be hashable")
	}
	if got := value.AttrNames(); strings.Join(got, ",") != "get,role,text" {
		t.Fatalf("attrs = %v", got)
	}
	if attr, err := value.Attr("missing"); err != nil || attr != nil {
		t.Fatalf("unknown attr = %v, %v", attr, err)
	}
	if _, found, err := value.Get(star.MakeInt(1)); err != nil || found {
		t.Fatal("a non-string key is not a field")
	}
	if empty := messageValueOf(message.NewTextMessage("", "")); empty.Truth() != star.False {
		t.Fatal("an empty message is falsy")
	}
}

func TestMessageGetHandlesKeysAndDefaults(t *testing.T) {
	value := messageValueOf(message.NewTextMessage(message.RoleAssistant, "旁白"))
	cases := []struct {
		name string
		args star.Tuple
		want star.Value
	}{
		{"field", star.Tuple{star.String("text")}, star.String("旁白")},
		{"default", star.Tuple{star.String("missing"), star.String("none")}, star.String("none")},
		{"no default", star.Tuple{star.String("missing")}, star.None},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := value.get(nil, nil, test.args, nil)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got != test.want {
				t.Fatalf("got %s, want %s", got, test.want)
			}
		})
	}
	if _, err := value.get(nil, nil, nil, nil); err == nil {
		t.Fatal("get requires a key")
	}
}

func TestMessageComparison(t *testing.T) {
	left := messageValueOf(message.NewTextMessage(message.RoleUser, "一"))
	same := messageValueOf(message.NewTextMessage(message.RoleUser, "一"))
	other := messageValueOf(message.NewTextMessage(message.RoleUser, "二"))
	if equal, err := left.CompareSameType(syntax.EQL, same, 0); err != nil || !equal {
		t.Fatalf("equal = %v, %v", equal, err)
	}
	if equal, err := left.CompareSameType(syntax.NEQ, other, 0); err != nil || !equal {
		t.Fatalf("not equal = %v, %v", equal, err)
	}
	if _, err := left.CompareSameType(syntax.EQL, star.String("x"), 0); err == nil {
		t.Fatal("comparing a message to another type must fail")
	}
	if _, err := left.CompareSameType(syntax.LT, same, 0); err == nil {
		t.Fatal("a message is not ordered")
	}
}

func TestViewInterfaceSurface(t *testing.T) {
	empty := view()
	if empty.String() != "[]" {
		t.Fatalf("empty string = %q", empty.String())
	}
	if empty.Truth() != star.False {
		t.Fatal("an empty transcript is falsy")
	}
	empty.Freeze()
	if _, err := empty.Hash(); err == nil {
		t.Fatal("a transcript must not be hashable")
	}
	if attr, err := empty.Attr("missing"); err != nil || attr != nil {
		t.Fatalf("unknown attr = %v, %v", attr, err)
	}
	if full := sampleView(); full.Truth() != star.True {
		t.Fatal("a non-empty transcript is truthy")
	}
}

func TestViewSlicesBothWays(t *testing.T) {
	value := sampleView()
	forward := value.Slice(0, 2, 1).(*star.List)
	if forward.Len() != 2 {
		t.Fatalf("forward = %d", forward.Len())
	}
	backward := value.Slice(2, -1, -1).(*star.List)
	if backward.Len() != 3 {
		t.Fatalf("backward = %d", backward.Len())
	}
	first, _ := backward.Index(0).(messageValue)
	if first.text != "三" {
		t.Fatalf("backward[0] = %q, want the last message first", first.text)
	}
}

func TestIteratorFinishes(t *testing.T) {
	iterator := sampleView().Iterate()
	var value star.Value
	count := 0
	for iterator.Next(&value) {
		count++
	}
	iterator.Done()
	if count != 3 {
		t.Fatalf("iterated %d messages", count)
	}
}
