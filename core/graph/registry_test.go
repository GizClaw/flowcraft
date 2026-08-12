package graph

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
)

func TestRegisterTypeValidation(t *testing.T) {
	reg := NewRegistry()

	if err := RegisterType(reg, "", echoNode(nil)); err == nil {
		t.Fatal("empty type name accepted")
	}
	if err := RegisterType[echoCfg](reg, "x", NodeType[echoCfg]{}); err == nil {
		t.Fatal("nil handler accepted")
	}

	badRole := echoNode(nil)
	badRole.Meta.Writes = []Role{{Kind: "bogus", Name: "v"}}
	if err := RegisterType(reg, "bad-kind", badRole); err == nil {
		t.Fatal("unknown role kind accepted")
	}

	bothKeys := echoNode(nil)
	bothKeys.Meta.Writes = []Role{{Kind: RoleVar, Name: "v", ConfigKey: "set_var"}}
	if err := RegisterType(reg, "both-keys", bothKeys); err == nil {
		t.Fatal("Name+ConfigKey both set accepted")
	}

	unknownField := echoNode(nil)
	unknownField.Meta.Writes = []Role{{Kind: RoleVar, ConfigKey: "not_a_field"}}
	if err := RegisterType(reg, "bad-config-key", unknownField); err == nil {
		t.Fatal("ConfigKey naming a non-field accepted")
	}

	if err := RegisterType(reg, "echo", echoNode(nil)); err != nil {
		t.Fatalf("valid registration rejected: %v", err)
	}
	if err := RegisterType(reg, "echo", echoNode(nil)); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	if !reg.Has("echo") || reg.Has("nope") {
		t.Fatal("Has wrong")
	}
	if meta, ok := reg.MetaOf("echo"); !ok || meta.Desc == "" {
		t.Fatal("MetaOf wrong")
	}
	if names := reg.TypeNames(); len(names) != 1 || names[0] != "echo" {
		t.Fatalf("TypeNames = %v", names)
	}
}

func TestFallbackHandler(t *testing.T) {
	reg := newTestRegistry(t)
	called := ""
	reg.RegisterFallback(func(ctx ExecutionContext, board *agent.Board, typeName string, raw json.RawMessage) error {
		called = typeName
		board.SetVar("fell_back", true)
		return nil
	})

	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "unregistered-type"}},
	}, reg)

	board := mustRun(t, g, agent.NewBoard())
	if called != "unregistered-type" {
		t.Fatalf("fallback not invoked, called=%q", called)
	}
	if v, _ := board.GetVar("fell_back"); v != true {
		t.Fatal("fallback did not run its handler")
	}
}

func TestFallbackNilRejected(t *testing.T) {
	reg := newTestRegistry(t)
	_, err := Build(&GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "unregistered-type"}},
	}, reg)
	if err == nil {
		t.Fatal("unregistered type without fallback accepted")
	}
}
