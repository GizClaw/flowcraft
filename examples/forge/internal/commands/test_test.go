package commands

import (
	"testing"
)

func TestParseTestFile(t *testing.T) {
	raw := []byte(`
name: werewolf_opening_setup
description: Starts a new Werewolf game.
raid: werewolf
turns:
  - 开始狼人杀
future: ignored
`)
	test, err := parseTestFile(raw)
	if err != nil {
		t.Fatalf("parseTestFile: %v", err)
	}
	if test.Name != "werewolf_opening_setup" || test.Raid != "werewolf" {
		t.Fatalf("test = %+v", test)
	}
	if len(test.Turns) != 1 || test.Turns[0] != "开始狼人杀" {
		t.Fatalf("turns = %+v", test.Turns)
	}
}

func TestParseTestFileRejectsDuplicateKeys(t *testing.T) {
	raw := []byte("name: one\nname: two\nraid: werewolf\n")
	if _, err := parseTestFile(raw); err == nil {
		t.Fatal("expected duplicate YAML keys to be rejected")
	}
}
