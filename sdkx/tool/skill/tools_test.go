package skill

import (
	"context"
	"strings"
	"testing"

	sdkskill "github.com/GizClaw/flowcraft/sdk/skill"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	skillworkspace "github.com/GizClaw/flowcraft/sdkx/skill/workspace"
)

func newTestTool(t *testing.T) *Tool {
	t.Helper()
	ws := sdkworkspace.NewMemWorkspace()
	write := func(path, body string) {
		t.Helper()
		if err := ws.Write(context.Background(), path, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write("skills/github/SKILL.md", `---
name: github
description: Review GitHub pull requests.
---
# GitHub

Use gh for PR reviews.
`)
	write("skills/summarize/SKILL.md", `---
name: summarize
description: Summarize documents.
---
# Summarize
`)
	return New(skillworkspace.New(ws))
}

func TestToolSearch(t *testing.T) {
	tool := newTestTool(t)
	out, err := tool.Execute(context.Background(), `{"action":"search","query":"github pull request review"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"name":"github"`) {
		t.Fatalf("search output = %s, want github", out)
	}
}

func TestToolInfo(t *testing.T) {
	tool := newTestTool(t)
	out, err := tool.Execute(context.Background(), `{"action":"info","name":"github"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# GitHub") {
		t.Fatalf("info output = %s, want skill body", out)
	}
}

func TestToolWhitelistFromOption(t *testing.T) {
	tool := newTestTool(t)
	tool.Whitelist = []string{"summarize"}
	if _, err := tool.Execute(context.Background(), `{"action":"info","name":"github"}`); err == nil {
		t.Fatal("info should fail for disallowed skill")
	}
}

func TestToolWhitelistFromContext(t *testing.T) {
	tool := newTestTool(t)
	ctx := sdkskill.WithWhitelist(context.Background(), []string{"summarize"})
	if _, err := tool.Execute(ctx, `{"action":"info","name":"github"}`); err == nil {
		t.Fatal("info should fail for context-disallowed skill")
	}
	out, err := tool.Execute(ctx, `{"action":"search","query":"github summarize"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, `"name":"github"`) || !strings.Contains(out, `"name":"summarize"`) {
		t.Fatalf("search output = %s, want only summarize", out)
	}
}

func TestToolValidation(t *testing.T) {
	tool := newTestTool(t)
	cases := []string{
		`{"action":"search"}`,
		`{"action":"info"}`,
		`{"action":"call","name":"github"}`,
		`{bad json`,
	}
	for _, input := range cases {
		if _, err := tool.Execute(context.Background(), input); err == nil {
			t.Fatalf("Execute(%s) should fail", input)
		}
	}
}

func TestToolMetadata(t *testing.T) {
	meta := newTestTool(t).Metadata()
	if meta.MutatesState {
		t.Fatal("skill search/info tool should not declare state mutation")
	}
}
