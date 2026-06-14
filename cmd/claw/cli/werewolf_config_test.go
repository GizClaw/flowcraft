package cli

import (
	"io/fs"
	"strings"
	"testing"
)

func TestWerewolfRaidConfigAvoidsFixedScriptShortcuts(t *testing.T) {
	raw, err := templateFS.ReadFile("examples/raids/werewolf.yaml")
	if err != nil {
		t.Fatalf("read werewolf raid: %v", err)
	}
	text := string(raw)
	if _, err := decodeConfigFile(raw); err != nil {
		t.Fatalf("decode werewolf raid: %v", err)
	}

	for _, forbidden := range []string{
		"fixed-test-seed",
		"Number(id) === 2",
		"本次必须报验",
		"pushSeerResultIfPossible",
		"publicSeerResults",
		"公开焦点：[^0-9]*",
		"女巫已经出局",
		"跳过女巫行动",
		"预言家已经出局",
		"跳过预言家行动",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("werewolf raid contains fixed shortcut %q", forbidden)
		}
	}
	if got := strings.Count(text, "seer_results:"); got != 1 {
		t.Fatalf("seer_results appears %d times, want only private state initialization", got)
	}
	for _, want := range []string{
		`const assignedUserRole = "villager";`,
		`"werewolf", "werewolf", "werewolf"`,
		"privateSeatContextFor",
		"state.vote_records.push",
		"查验([1-8])号",
		"票型：",
		"身份暂不公开",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("werewolf raid missing %q", want)
		}
	}
	for _, want := range []string{
		"只能自称4号小满",
		"只能自称5号周岚",
		"只能自称6号陈医生",
		"只能自称7号老赵",
		"只能自称8号苏禾",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("werewolf raid missing speaker guard %q", want)
		}
	}
}

func TestWerewolfExampleTestsAvoidFixedRoleScript(t *testing.T) {
	entries, err := fs.ReadDir(templateFS, "examples/test/werewolf")
	if err != nil {
		t.Fatalf("read werewolf tests: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := templateFS.ReadFile("examples/test/werewolf/" + entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		text := string(raw)
		for _, forbidden := range []string{
			"真预言家",
			"先跟票出1号",
			"2号第二天",
			"2号第二天又报",
			"2号第二天报",
		} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s contains fixed role script %q", entry.Name(), forbidden)
			}
		}
	}
}
