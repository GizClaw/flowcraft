package workspace

import (
	"context"
	"os"
	"runtime"
	"testing"

	sdkskill "github.com/GizClaw/flowcraft/sdk/skill"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func writeSkill(t *testing.T, ws sdkworkspace.Workspace, dir, body string) {
	t.Helper()
	if err := ws.Write(context.Background(), "skills/"+dir+"/SKILL.md", []byte(body)); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogSearchAndLoad(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	writeSkill(t, ws, "github", `---
name: github
description: Review pull requests and work with GitHub issues.
tags: [git, review]
metadata:
  openclaw:
    requires:
      bins: [gh]
---
# GitHub Skill

Use gh to inspect pull requests.
`)
	writeSkill(t, ws, "summarize", `---
name: summarize
description: Summarize documents and conversations.
---
# Summarize
`)

	catalog := New(ws)
	results, err := catalog.Search(context.Background(), "review github pull request", sdkskill.SearchOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Name != "github" {
		t.Fatalf("Search() = %#v, want github first", results)
	}

	loaded, err := catalog.Load(context.Background(), "github")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Path != "skills/github/SKILL.md" {
		t.Fatalf("Path = %q", loaded.Path)
	}
	if loaded.Requires == nil || len(loaded.Requires.Bins) != 1 || loaded.Requires.Bins[0] != "gh" {
		t.Fatalf("Requires = %#v, want gh binary", loaded.Requires)
	}
}

func TestCatalogListWhitelistAndRefresh(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	writeSkill(t, ws, "one", "---\nname: one\ndescription: first skill\n---\n# One\n")
	writeSkill(t, ws, "two", "---\nname: two\ndescription: second skill\n---\n# Two\n")

	catalog := New(ws)
	got, err := catalog.List(context.Background(), sdkskill.ListOptions{Whitelist: []string{"two"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "two" {
		t.Fatalf("List whitelist = %#v, want two only", got)
	}

	writeSkill(t, ws, "three", "---\nname: three\ndescription: third skill\n---\n# Three\n")
	if err := catalog.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err = catalog.List(context.Background(), sdkskill.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("List after Refresh len = %d, want 3", len(got))
	}
}

func TestCatalogGating(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	writeSkill(t, ws, "gated", `---
name: gated
description: Needs missing dependencies.
requires:
  bins: [definitely_missing_flowcraft_test_bin]
  env: [FLOWCRAFT_SKILL_TEST_ENV_MISSING]
---
# Gated
`)

	catalog := New(ws)
	sk, err := catalog.Load(context.Background(), "gated")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Available {
		t.Fatal("expected gated skill to be unavailable")
	}
	if len(sk.MissingDeps) < 2 {
		t.Fatalf("MissingDeps = %#v, want bin and env", sk.MissingDeps)
	}
}

func TestCatalogOSGating(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	otherOS := "linux"
	if runtime.GOOS == "linux" {
		otherOS = "darwin"
	}
	writeSkill(t, ws, "os-gated", "---\nname: os-gated\ndescription: OS gated.\nrequires:\n  os: ["+otherOS+"]\n---\n# OS\n")

	catalog := New(ws)
	sk, err := catalog.Load(context.Background(), "os-gated")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Available {
		t.Fatal("expected unsupported OS skill to be unavailable")
	}
	if len(sk.MissingDeps) != 1 || sk.MissingDeps[0] == "" {
		t.Fatalf("MissingDeps = %#v, want reason", sk.MissingDeps)
	}
}

func TestCatalogEnvGatingAvailable(t *testing.T) {
	t.Setenv("FLOWCRAFT_SKILL_TEST_ENV_PRESENT", "1")
	ws := sdkworkspace.NewMemWorkspace()
	writeSkill(t, ws, "env-ok", `---
name: env-ok
description: Env is present.
requires:
  env: [FLOWCRAFT_SKILL_TEST_ENV_PRESENT]
---
# OK
`)

	catalog := New(ws)
	sk, err := catalog.Load(context.Background(), "env-ok")
	if err != nil {
		t.Fatal(err)
	}
	if !sk.Available {
		t.Fatalf("expected available, missing=%#v", sk.MissingDeps)
	}
}

func TestCatalogIgnoresInvalidSkills(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	if err := ws.Write(context.Background(), "skills/bad/SKILL.md", []byte("# no frontmatter")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, ws, "good", "---\nname: good\ndescription: Good skill.\n---\n# Good\n")

	catalog := New(ws)
	got, err := catalog.List(context.Background(), sdkskill.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("List() = %#v, want only good", got)
	}
}

func TestCatalogMissingRootIsEmpty(t *testing.T) {
	catalog := New(sdkworkspace.NewMemWorkspace())
	got, err := catalog.List(context.Background(), sdkskill.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("List() len = %d, want 0", len(got))
	}
}

func TestCatalogDoesNotLeakMutableSlices(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	writeSkill(t, ws, "tagged", "---\nname: tagged\ndescription: Tagged skill.\ntags: [one]\n---\n# Tagged\n")
	catalog := New(ws)

	first, err := catalog.List(context.Background(), sdkskill.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first[0].Tags[0] = "mutated"
	second, err := catalog.List(context.Background(), sdkskill.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Tags[0] != "one" {
		t.Fatalf("Tags mutated through List result: %#v", second[0].Tags)
	}
}

func TestCatalogIgnoresFilesAtRoot(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	if err := ws.Write(context.Background(), "skills/README.md", []byte("not a skill")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, ws, "actual", "---\nname: actual\ndescription: Actual skill.\n---\n# Actual\n")
	catalog := New(ws)
	got, err := catalog.List(context.Background(), sdkskill.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "actual" {
		t.Fatalf("List() = %#v", got)
	}
}

func TestCatalogAnyBinsGatingUsesOneInstalledBinary(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	writeSkill(t, ws, "any-bin", `---
name: any-bin
description: Needs any shell.
requires:
  any_bins: [definitely_missing_flowcraft_test_bin, `+shellBinary()+`]
---
# Any
`)
	catalog := New(ws)
	sk, err := catalog.Load(context.Background(), "any-bin")
	if err != nil {
		t.Fatal(err)
	}
	if !sk.Available {
		t.Fatalf("expected any-bin skill to be available, missing=%#v", sk.MissingDeps)
	}
}

func shellBinary() string {
	if _, err := os.Stat("/bin/sh"); err == nil {
		return "sh"
	}
	return "go"
}
