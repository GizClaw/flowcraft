package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadChangesetsRejectsInvalidContracts(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty summary", `{"summary":" ","releases":[{"module":"core","bump":"patch"}]}`, "summary"},
		{"invalid module", `{"summary":"x","releases":[{"module":"unknown","bump":"patch"}]}`, "module"},
		{"invalid bump", `{"summary":"x","releases":[{"module":"core","bump":"major"}]}`, "bump"},
		{"duplicate module", `{"summary":"x","releases":[{"module":"core","bump":"patch"},{"module":"core","bump":"minor"}]}`, "duplicate"},
		{"unknown field", `{"summary":"x","releases":[{"module":"core","bump":"patch"}],"extra":true}`, "unknown field"},
		{"empty releases", `{"summary":"x","releases":[]}`, "release"},
		{"multiline summary", "{\"summary\":\"first\\nsecond\",\"releases\":[{\"module\":\"core\",\"bump\":\"patch\"}]}", "single line"},
		{"reserved marker", `{"summary":"break <!-- releasegate:releases --> parsing","releases":[{"module":"core","bump":"patch"}]}`, "reserved"},
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
	writeFile(t, repo, ".release/existing.json", validChangeset("old", "core", "patch"))
	writeFile(t, repo, ".release/README.md", "old docs\n")
	commitAll(t, repo, "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")

	writeFile(t, repo, ".release/existing.json", validChangeset("changed", "core", "minor"))
	writeFile(t, repo, ".release/new.json", validChangeset("new", "core", "patch"))
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
}

func TestPlanAggregatesHighestBump(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "core", "")
	commitAll(t, repo, "seed")
	gitRun(t, repo, "tag", "core/v1.2.3")

	writeFile(t, repo, "core/change.go", "package core\n")
	writeFile(t, repo, ".release/patch.json", validChangeset("patch", "core", "patch"))
	writeFile(t, repo, ".release/minor.json", validChangeset("minor", "core", "minor"))

	got, err := buildPlan(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Modules) != 1 {
		t.Fatalf("modules = %#v, want one", got.Modules)
	}
	module := got.Modules[0]
	if module.Bump != "minor" || module.Current != "1.2.3" || module.Next != "1.3.0" || module.Tag != "core/v1.3.0" {
		t.Fatalf("module = %#v", module)
	}
	if strings.Join(module.Replaces, ",") != ".release/minor.json,.release/patch.json" {
		t.Fatalf("replaces = %v", module.Replaces)
	}
}

func TestPlanTreatsChangesetInLatestTagAsConsumed(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "core", "")
	writeFile(t, repo, ".release/done.json", validChangeset("done", "core", "patch"))
	commitAll(t, repo, "released")
	gitRun(t, repo, "tag", "core/v1.0.0")
	writeFile(t, repo, "core/later.go", "package core\n")

	got, err := buildPlan(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Modules) != 0 || len(got.Tags) != 0 {
		t.Fatalf("plan = %#v, want empty", got)
	}
}

func TestPlanRejectsMissingSeedTag(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "core", "")
	writeFile(t, repo, ".release/change.json", validChangeset("change", "core", "patch"))
	commitAll(t, repo, "change")

	_, err := buildPlan(repo)
	if err == nil || !strings.Contains(err.Error(), "seed tag") {
		t.Fatalf("buildPlan() error = %v, want seed tag error", err)
	}
}

func TestPlanRejectsModuleWithoutChangesSinceTag(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "core", "")
	commitAll(t, repo, "seed")
	gitRun(t, repo, "tag", "core/v1.0.0")
	writeFile(t, repo, ".release/change.json", validChangeset("change", "core", "patch"))

	_, err := buildPlan(repo)
	if err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("buildPlan() error = %v, want no changes error", err)
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
	if got.Modules == nil || got.Tags == nil {
		t.Fatalf("empty plan must use JSON arrays: %#v", got)
	}
	if len(got.Modules)+len(got.Tags) != 0 {
		t.Fatalf("plan = %#v, want empty", got)
	}
}

func TestChangelogUpdatesPublishedStateRow(t *testing.T) {
	before := "# Changelog\n\n" +
		"## Current Published State\n\n" +
		"| Module | Latest tag | Notes |\n" +
		"| --- | --- | --- |\n" +
		"| `core` | `core/v0.1.0` | Active. |\n" +
		"| `vessel` | `vessel/v0.3.0` | Retired. |\n\n" +
		"## [Unreleased]\n"
	got, err := updatePublishedState(before, []ModulePlan{{Module: "core", Tag: "core/v0.2.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "| `core` | `core/v0.2.0` | Active. |") {
		t.Fatalf("core row not updated:\n%s", got)
	}
	if !strings.Contains(got, "| `vessel` | `vessel/v0.3.0` | Retired. |") {
		t.Fatalf("retired row changed:\n%s", got)
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
