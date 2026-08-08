package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadChangesetsRejectsInvalidContracts(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty summary", `{"summary":" ","releases":[{"module":"sdk","bump":"patch"}]}`, "summary"},
		{"invalid module", `{"summary":"x","releases":[{"module":"unknown","bump":"patch"}]}`, "module"},
		{"invalid bump", `{"summary":"x","releases":[{"module":"sdk","bump":"major"}]}`, "bump"},
		{"duplicate module", `{"summary":"x","releases":[{"module":"sdk","bump":"patch"},{"module":"sdk","bump":"minor"}]}`, "duplicate"},
		{"unknown field", `{"summary":"x","releases":[{"module":"sdk","bump":"patch"}],"extra":true}`, "unknown field"},
		{"empty releases", `{"summary":"x","releases":[]}`, "release"},
		{"multiline summary", "{\"summary\":\"first\\nsecond\",\"releases\":[{\"module\":\"sdk\",\"bump\":\"patch\"}]}", "single line"},
		{"reserved marker", `{"summary":"break <!-- releasegate:releases --> parsing","releases":[{"module":"sdk","bump":"patch"}]}`, "reserved"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			writeFile(t, repo, ".release/change.json", tt.body)
			_, err := loadChangesets(repo)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tt.want) {
				t.Fatalf("loadChangesets() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateBaseAllowsOnlyAddedChangesets(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, ".release/existing.json", validChangeset("old", "sdk", "patch"))
	writeFile(t, repo, ".release/README.md", "old docs\n")
	commitAll(t, repo, "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")

	writeFile(t, repo, ".release/existing.json", validChangeset("changed", "sdk", "minor"))
	writeFile(t, repo, ".release/new.json", validChangeset("new", "memory", "patch"))
	writeFile(t, repo, ".release/README.md", "new docs\n")
	if err := validateRepo(repo, base); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("validateRepo() error = %v, want immutable modification error", err)
	}

	gitRun(t, repo, "restore", ".release/existing.json")
	gitRun(t, repo, "add", ".release/new.json")
	if err := validateRepo(repo, base); err != nil {
		t.Fatalf("validateRepo() unexpected error for addition: %v", err)
	}

	gitRun(t, repo, "rm", ".release/existing.json")
	if err := validateRepo(repo, base); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("validateRepo() error = %v, want immutable deletion error", err)
	}

	gitRun(t, repo, "restore", "--staged", "--worktree", ".release/existing.json")
	gitRun(t, repo, "mv", ".release/existing.json", ".release/existing.md")
	if err := validateRepo(repo, base); err == nil || !strings.Contains(err.Error(), "renamed") {
		t.Fatalf("validateRepo() error = %v, want immutable rename error", err)
	}
}

func TestPlanAggregatesHighestBump(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "sdk", "")
	commitAll(t, repo, "seed")
	gitRun(t, repo, "tag", "sdk/v1.2.3")

	writeFile(t, repo, "sdk/change.go", "package sdk\n")
	writeFile(t, repo, ".release/patch.json", validChangeset("patch", "sdk", "patch"))
	writeFile(t, repo, ".release/minor.json", validChangeset("minor", "sdk", "minor"))

	got, err := buildPlan(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Modules) != 1 {
		t.Fatalf("modules = %#v, want one", got.Modules)
	}
	module := got.Modules[0]
	if module.Bump != "minor" || module.Current != "1.2.3" || module.Next != "1.3.0" || module.Tag != "sdk/v1.3.0" {
		t.Fatalf("module = %#v", module)
	}
	if strings.Join(module.Replaces, ",") != ".release/minor.json,.release/patch.json" {
		t.Fatalf("replaces = %v", module.Replaces)
	}
}

func TestPlanTreatsChangesetInLatestTagAsConsumed(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "sdk", "")
	writeFile(t, repo, ".release/done.json", validChangeset("done", "sdk", "patch"))
	commitAll(t, repo, "released")
	gitRun(t, repo, "tag", "sdk/v1.0.0")
	writeFile(t, repo, "sdk/later.go", "package sdk\n")

	got, err := buildPlan(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Modules) != 0 || len(got.Matrix.Include) != 0 || len(got.Tags) != 0 {
		t.Fatalf("plan = %#v, want empty", got)
	}
}

func TestPlanRejectsMissingSeedTag(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "sdk", "")
	writeFile(t, repo, ".release/change.json", validChangeset("change", "sdk", "patch"))
	commitAll(t, repo, "change")

	_, err := buildPlan(repo)
	if err == nil || !strings.Contains(err.Error(), "seed tag") {
		t.Fatalf("buildPlan() error = %v, want seed tag error", err)
	}
}

func TestPlanRejectsModuleWithoutChangesSinceTag(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "sdk", "")
	commitAll(t, repo, "seed")
	gitRun(t, repo, "tag", "sdk/v1.0.0")
	writeFile(t, repo, ".release/change.json", validChangeset("change", "sdk", "patch"))

	_, err := buildPlan(repo)
	if err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("buildPlan() error = %v, want no changes error", err)
	}
}

func TestPlanUsesDependencyOrder(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "sdk", "")
	seedModule(t, repo, "memory", "require github.com/GizClaw/flowcraft/sdk v1.0.0\n")
	seedModule(t, repo, "sdkx", "require (\n github.com/GizClaw/flowcraft/sdk v1.0.0\n github.com/GizClaw/flowcraft/memory v1.0.0\n)\n")
	commitAll(t, repo, "seed")
	for _, module := range []string{"sdk", "memory", "sdkx"} {
		gitRun(t, repo, "tag", module+"/v1.0.0")
	}

	for _, module := range []string{"sdk", "memory", "sdkx"} {
		writeFile(t, repo, module+"/change.go", "package "+module+"\n")
	}
	writeFile(t, repo, "memory/go.mod", moduleFile("memory", "require github.com/GizClaw/flowcraft/sdk v1.0.1\n"))
	writeFile(t, repo, "sdkx/go.mod", moduleFile("sdkx", "require (\n github.com/GizClaw/flowcraft/sdk v1.0.1\n github.com/GizClaw/flowcraft/memory v1.0.1\n)\n"))
	writeFile(t, repo, ".release/all.json", `{"summary":"all","releases":[{"module":"sdkx","bump":"patch"},{"module":"memory","bump":"patch"},{"module":"sdk","bump":"patch"}]}`)

	got, err := buildPlan(repo)
	if err != nil {
		t.Fatal(err)
	}
	var modules []string
	for _, item := range got.Modules {
		modules = append(modules, item.Module)
	}
	if strings.Join(modules, ",") != "sdk,memory,sdkx" {
		t.Fatalf("module order = %v", modules)
	}
}

func TestPlanRejectsIncorrectSameBatchDependencyPin(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "sdk", "")
	seedModule(t, repo, "memory", "require github.com/GizClaw/flowcraft/sdk v1.0.0\n")
	commitAll(t, repo, "seed")
	gitRun(t, repo, "tag", "sdk/v1.0.0")
	gitRun(t, repo, "tag", "memory/v1.0.0")
	writeFile(t, repo, "sdk/change.go", "package sdk\n")
	writeFile(t, repo, "memory/change.go", "package memory\n")
	writeFile(t, repo, ".release/all.json", `{"summary":"all","releases":[{"module":"sdk","bump":"minor"},{"module":"memory","bump":"patch"}]}`)

	_, err := buildPlan(repo)
	if err == nil || !strings.Contains(err.Error(), "v1.1.0") {
		t.Fatalf("buildPlan() error = %v, want next-version pin error", err)
	}
}

func TestPlanEmptySetAndCLIJSON(t *testing.T) {
	repo := initRepo(t)
	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"plan", "--repo", repo, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("runCLI() code = %d, stderr = %s", code, stderr.String())
	}
	var got Plan
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", stdout.String(), err)
	}
	if got.Modules == nil || got.Matrix.Include == nil || got.Tags == nil {
		t.Fatalf("empty plan must use JSON arrays: %#v", got)
	}
	if len(got.Modules)+len(got.Matrix.Include)+len(got.Tags) != 0 {
		t.Fatalf("plan = %#v, want empty", got)
	}
}

func TestChangelogSingleModuleUsesPlanTagAndUpdatesState(t *testing.T) {
	repo := changelogRepo(t, "sdk", "1.2.3")
	writeFile(t, repo, "sdk/change.go", "package sdk\n\nconst Changed = true\n")
	writeFile(t, repo, ".release/z-patch.json", validChangeset("patch fix", "sdk", "patch"))
	writeFile(t, repo, ".release/a-minor.json", validChangeset("minor feature", "sdk", "minor"))
	commitAllAt(t, repo, "changes", "2026-07-25T12:00:00Z")

	got, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	assertContains(t, text, "| `sdk` | `sdk/v1.3.0` |")
	assertContains(t, text, "## `sdk/v1.3.0` - 2026-07-25\n\n### Changed\n\n- minor feature\n- patch fix\n")
	if strings.Index(text, "- minor feature") > strings.Index(text, "- patch fix") {
		t.Fatal("bullets must follow stable changeset filename order")
	}
}

func TestChangelogAssignsMultiModuleSummariesAndDeduplicates(t *testing.T) {
	repo := changelogRepo(t, "sdk", "1.0.0")
	seedModule(t, repo, "memory", "require github.com/GizClaw/flowcraft/sdk v1.0.0\n")
	commitAll(t, repo, "seed memory")
	gitRun(t, repo, "tag", "memory/v2.0.0")
	changelogPath := filepath.Join(repo, "CHANGELOG.md")
	changelog, err := os.ReadFile(changelogPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "CHANGELOG.md", strings.Replace(string(changelog), "| `vessel`", "| `memory` | `memory/v2.0.0` | Active. |\n| `vessel`", 1))
	writeFile(t, repo, "sdk/change.go", "package sdk\n\nconst Changed = true\n")
	writeFile(t, repo, "memory/change.go", "package memory\n\nconst Changed = true\n")
	writeFile(t, repo, "memory/go.mod", moduleFile("memory", "require github.com/GizClaw/flowcraft/sdk v1.0.1\n"))
	writeFile(t, repo, ".release/a.json", `{"summary":"shared","releases":[{"module":"sdk","bump":"patch"},{"module":"memory","bump":"patch"}]}`)
	writeFile(t, repo, ".release/b.json", validChangeset("sdk only", "sdk", "patch"))
	writeFile(t, repo, ".release/c.json", validChangeset("shared", "sdk", "patch"))
	commitAllAt(t, repo, "changes", "2026-07-24T12:00:00Z")

	got, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	sdk := changelogSection(t, text, "sdk/v1.0.1")
	memory := changelogSection(t, text, "memory/v2.0.1")
	if strings.Count(sdk, "- shared\n") != 1 || !strings.Contains(sdk, "- sdk only\n") {
		t.Fatalf("sdk section = %q", sdk)
	}
	if strings.Count(memory, "- shared\n") != 1 || strings.Contains(memory, "sdk only") {
		t.Fatalf("memory section = %q", memory)
	}
}

func TestChangelogUsesLatestChangesetCommitDate(t *testing.T) {
	repo := changelogRepo(t, "sdk", "1.0.0")
	writeFile(t, repo, "sdk/change.go", "package sdk\n\nconst Changed = true\n")
	writeFile(t, repo, ".release/a.json", validChangeset("first", "sdk", "patch"))
	commitAllAt(t, repo, "first", "2026-07-20T12:00:00Z")
	writeFile(t, repo, ".release/b.json", validChangeset("second", "sdk", "patch"))
	commitAllAt(t, repo, "second", "2026-07-26T12:00:00Z")

	got, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, string(got), "## `sdk/v1.0.1` - 2026-07-26")
}

func TestChangelogIsIdempotentAndRepairsExistingSection(t *testing.T) {
	repo := changelogRepo(t, "sdk", "1.0.0")
	writeFile(t, repo, "sdk/change.go", "package sdk\n\nconst Changed = true\n")
	writeFile(t, repo, ".release/a.json", validChangeset("change", "sdk", "patch"))
	commitAllAt(t, repo, "change", "2026-07-25T12:00:00Z")

	first, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "CHANGELOG.md", string(first))
	second, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("second changelog generation changed content")
	}

	writeFile(t, repo, "CHANGELOG.md", strings.Replace(string(first), "- change", "- wrong", 1))
	repaired, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, repaired) {
		t.Fatalf("repaired changelog differs from expected:\n%s", repaired)
	}
}

func TestChangelogRebuildsUntaggedSectionWhenSummaryAndDateChange(t *testing.T) {
	repo := changelogRepo(t, "sdk", "1.0.0")
	writeFile(t, repo, "sdk/change.go", "package sdk\n\nconst Changed = true\n")
	writeFile(t, repo, ".release/a.json", validChangeset("first", "sdk", "patch"))
	commitAllAt(t, repo, "first", "2026-07-20T12:00:00Z")

	first, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "CHANGELOG.md", string(first))
	writeFile(t, repo, ".release/b.json", validChangeset("second", "sdk", "patch"))
	commitAllAt(t, repo, "second", "2026-07-26T12:00:00Z")

	got, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Count(text, "## `sdk/v1.0.1`") != 1 {
		t.Fatalf("expected one rebuilt section:\n%s", text)
	}
	assertContains(t, text, "## `sdk/v1.0.1` - 2026-07-26\n\n### Changed\n\n- first\n- second\n")
	writeFile(t, repo, "CHANGELOG.md", text)
	again, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, again) {
		t.Fatal("rebuilt changelog is not byte-idempotent")
	}
}

func TestChangelogRemovesOldUntaggedPatchSectionAfterMinorBump(t *testing.T) {
	repo := changelogRepo(t, "sdk", "1.0.0")
	writeFile(t, repo, "sdk/change.go", "package sdk\n\nconst Changed = true\n")
	writeFile(t, repo, ".release/a.json", validChangeset("patch", "sdk", "patch"))
	commitAllAt(t, repo, "patch", "2026-07-20T12:00:00Z")

	patchChangelog, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "CHANGELOG.md", string(patchChangelog))
	writeFile(t, repo, ".release/b.json", validChangeset("minor", "sdk", "minor"))
	commitAllAt(t, repo, "minor", "2026-07-26T12:00:00Z")

	got, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "## `sdk/v1.0.1`") {
		t.Fatalf("stale untagged patch section remains:\n%s", text)
	}
	assertContains(t, text, "## `sdk/v1.1.0` - 2026-07-26\n\n### Changed\n\n- patch\n- minor\n")
	assertContains(t, text, "| `sdk` | `sdk/v1.1.0` |")
}

func TestChangelogPreservesTaggedHistoricalSection(t *testing.T) {
	repo := changelogRepo(t, "sdk", "1.0.0")
	writeFile(t, repo, "sdk/change.go", "package sdk\n\nconst First = true\n")
	writeFile(t, repo, ".release/a.json", validChangeset("first", "sdk", "patch"))
	commitAllAt(t, repo, "first", "2026-07-20T12:00:00Z")

	released, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	taggedSection := changelogSection(t, string(released), "sdk/v1.0.1")
	writeFile(t, repo, "CHANGELOG.md", string(released))
	commitAllAt(t, repo, "release changelog", "2026-07-21T12:00:00Z")
	gitRun(t, repo, "tag", "sdk/v1.0.1")

	writeFile(t, repo, "sdk/change.go", "package sdk\n\nconst First = true\nconst Second = true\n")
	writeFile(t, repo, ".release/b.json", validChangeset("second", "sdk", "patch"))
	commitAllAt(t, repo, "second", "2026-07-26T12:00:00Z")

	got, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if section := changelogSection(t, text, "sdk/v1.0.1"); section != taggedSection {
		t.Fatalf("tagged section changed:\nwant:\n%s\ngot:\n%s", taggedSection, section)
	}
	assertContains(t, text, "## `sdk/v1.0.2` - 2026-07-26\n\n### Changed\n\n- second\n")
}

func TestChangelogRejectsMissingMarkerAndUncommittedChangeset(t *testing.T) {
	t.Run("missing marker", func(t *testing.T) {
		repo := changelogRepo(t, "sdk", "1.0.0")
		writeFile(t, repo, "CHANGELOG.md", strings.Replace(changelogFixture("sdk", "1.0.0"), changelogMarker+"\n", "", 1))
		writeFile(t, repo, "sdk/change.go", "package sdk\n\nconst Changed = true\n")
		writeFile(t, repo, ".release/a.json", validChangeset("change", "sdk", "patch"))
		commitAllAt(t, repo, "change", "2026-07-25T12:00:00Z")
		if _, err := buildChangelog(repo); err == nil || !strings.Contains(err.Error(), "marker") {
			t.Fatalf("buildChangelog() error = %v, want marker error", err)
		}
	})

	t.Run("uncommitted changeset", func(t *testing.T) {
		repo := changelogRepo(t, "sdk", "1.0.0")
		writeFile(t, repo, "sdk/change.go", "package sdk\n\nconst Changed = true\n")
		writeFile(t, repo, ".release/a.json", validChangeset("change", "sdk", "patch"))
		if _, err := buildChangelog(repo); err == nil || !strings.Contains(err.Error(), "commit date") {
			t.Fatalf("buildChangelog() error = %v, want commit date error", err)
		}
	})
}

func TestChangelogEmptyPlanIsNoOp(t *testing.T) {
	repo := initRepo(t)
	body := "# Changelog\n\nNo release marker is needed yet.\n"
	writeFile(t, repo, "CHANGELOG.md", body)
	got, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("buildChangelog() = %q, want unchanged", got)
	}
}

func TestChangelogCLIPrintCheckAndWrite(t *testing.T) {
	repo := changelogRepo(t, "sdk", "1.0.0")
	writeFile(t, repo, "sdk/change.go", "package sdk\n\nconst Changed = true\n")
	writeFile(t, repo, ".release/a.json", validChangeset("change", "sdk", "patch"))
	commitAllAt(t, repo, "change", "2026-07-25T12:00:00Z")

	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"changelog", "--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("print code = %d, stderr = %s", code, stderr.String())
	}
	expected := stdout.String()
	if code := runCLI([]string{"changelog", "--repo", repo, "--check"}, &stdout, &stderr); code == 0 ||
		!strings.Contains(stderr.String(), "--write") {
		t.Fatalf("check should fail before write: code=%d stderr=%q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLI([]string{"changelog", "--repo", repo, "--write"}, &stdout, &stderr); code != 0 {
		t.Fatalf("write code = %d, stderr = %s", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(repo, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("written changelog differs from printed output")
	}
	if code := runCLI([]string{"changelog", "--repo", repo, "--check"}, &stdout, &stderr); code != 0 {
		t.Fatalf("check after write code = %d, stderr = %s", code, stderr.String())
	}
	if code := runCLI([]string{"changelog", "--repo", repo, "--check", "--write"}, &stdout, &stderr); code != 2 {
		t.Fatalf("mutually exclusive flags code = %d, want 2", code)
	}
}

func TestChangelogUpdatesPaddedPublishedStateRow(t *testing.T) {
	before := "# Changelog\n\n" +
		"## Current Published State\n\n" +
		"| Module   | Latest tag      | Notes\n" +
		"| -------- | --------------- | ----------------------------------------------------------------------------------------- |\n" +
		"| `sdk`    | `sdk/v0.5.0`    | Core agent, engine, graph, LLM, tool, workspace, event, and telemetry primitives.         |\n" +
		"| `memory` | `memory/v0.1.7` | Standalone memory-domain module: recall, history, knowledge, retrieval, text, and stores. |\n" +
		"| `sdkx`   | `sdkx/v0.4.10`  | Provider/adaptor release pinned to `sdk v0.4.8` and `memory v0.1.7`.                      |\n" +
		"\n## [Unreleased]\n"
	got, err := updatePublishedState(before, []ModulePlan{{Module: "sdkx", Tag: "sdkx/v0.5.0"}})
	if err != nil {
		t.Fatal(err)
	}
	wantSdkx := "| `sdkx`   | `sdkx/v0.5.0`  | Provider/adaptor release pinned to `sdk v0.4.8` and `memory v0.1.7`.                      |"
	if !strings.Contains(got, wantSdkx) {
		t.Fatalf("padded sdkx row not updated with padding preserved:\n%s", got)
	}
	if !strings.Contains(got, "| `sdk`    | `sdk/v0.5.0`    |") {
		t.Fatalf("unrelated sdk row changed:\n%s", got)
	}
	if !strings.Contains(got, "| `memory` | `memory/v0.1.7` |") {
		t.Fatalf("unrelated memory row changed:\n%s", got)
	}
}

func TestChangelogOnlyUpdatesActiveModuleRows(t *testing.T) {
	repo := changelogRepo(t, "sdk", "1.0.0")
	writeFile(t, repo, "sdk/change.go", "package sdk\n\nconst Changed = true\n")
	writeFile(t, repo, ".release/a.json", validChangeset("change", "sdk", "patch"))
	commitAllAt(t, repo, "change", "2026-07-25T12:00:00Z")
	before := changelogFixture("sdk", "1.0.0")
	got, err := buildChangelog(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(
		tableLine(string(got), "`vessel`"),
		tableLine(before, "`vessel`"),
	) {
		t.Fatal("retired row changed")
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "config", "user.email", "releasegate@example.com")
	gitRun(t, repo, "config", "user.name", "Release Gate")
	writeFile(t, repo, ".gitkeep", "")
	commitAll(t, repo, "initial")
	return repo
}

func seedModule(t *testing.T, repo, module, requirements string) {
	t.Helper()
	writeFile(t, repo, module+"/go.mod", moduleFile(module, requirements))
	writeFile(t, repo, module+"/module.go", "package "+module+"\n")
}

func moduleFile(module, requirements string) string {
	return "module github.com/GizClaw/flowcraft/" + module + "\n\ngo 1.25.0\n\n" + requirements
}

func validChangeset(summary, module, bump string) string {
	return `{"summary":"` + summary + `","releases":[{"module":"` + module + `","bump":"` + bump + `"}]}`
}

func writeFile(t *testing.T, repo, name, body string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, repo, message string) {
	t.Helper()
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", message)
}

func commitAllAt(t *testing.T, repo, message, date string) {
	t.Helper()
	gitRun(t, repo, "add", "-A")
	cmd := exec.Command("git", "-C", repo, "commit", "-q", "-m", message)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, output)
	}
}

func changelogRepo(t *testing.T, module, version string) string {
	t.Helper()
	repo := initRepo(t)
	seedModule(t, repo, module, "")
	writeFile(t, repo, "CHANGELOG.md", changelogFixture(module, version))
	commitAll(t, repo, "seed module and changelog")
	if module != "sdk" || version != "0.1.0" {
		gitRun(t, repo, "tag", "sdk/v0.1.0")
	}
	gitRun(t, repo, "tag", module+"/v"+version)
	return repo
}

func changelogFixture(module, version string) string {
	return "# Changelog\n\n" +
		"## Current Published State\n\n" +
		"| Module | Latest tag | Notes |\n" +
		"| --- | --- | --- |\n" +
		"| `" + module + "` | `" + module + "/v" + version + "` | Active. |\n" +
		"| `vessel` | `vessel/v0.3.0` | Retired. |\n\n" +
		"## [Unreleased]\n\n- Pending notes stay here.\n\n" +
		changelogMarker + "\n\n" +
		"## `sdk/v0.1.0` - 2026-01-01\n\nHistorical text must stay byte-for-byte unchanged.\n"
}

func changelogSection(t *testing.T, changelog, tag string) string {
	t.Helper()
	start := strings.Index(changelog, "## `"+tag+"`")
	if start < 0 {
		t.Fatalf("missing section for %s", tag)
	}
	rest := changelog[start:]
	if next := strings.Index(rest[3:], "\n## "); next >= 0 {
		return rest[:next+3]
	}
	return rest
}

func tableLine(body, module string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "| "+module+" |") {
			return line
		}
	}
	return ""
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("missing %q in:\n%s", want, got)
	}
}

func gitRun(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
