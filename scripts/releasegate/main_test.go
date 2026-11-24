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

func TestPlanInitialReleaseWithoutSeedTag(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "core", "")
	writeFile(t, repo, ".release/change.json", validChangeset("change", "core", "patch"))
	commitAll(t, repo, "change")

	got, err := buildPlan(repo)
	if err != nil {
		t.Fatalf("buildPlan() error = %v", err)
	}
	if len(got.Modules) != 1 || got.Modules[0].Current != "0.0.0" || got.Modules[0].Next != "0.0.1" {
		t.Fatalf("plan = %#v, want initial patch release from 0.0.0", got.Modules)
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

func TestPlanReleasesCraftAfterCore(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "core", "")
	seedModule(t, repo, "craft", "require github.com/GizClaw/flowcraft/core v1.2.3\n")
	commitAll(t, repo, "seed")
	gitRun(t, repo, "tag", "core/v1.2.3")

	writeFile(t, repo, "core/change.go", "package core\n")
	writeFile(t, repo, "craft/change.go",
		"package craft\n\nimport _ \"github.com/GizClaw/flowcraft/core\"\n")
	writeFile(t, repo, ".release/core.json", validChangeset("core change", "core", "patch"))
	writeFile(t, repo, ".release/craft.json", validChangeset("craft change", "craft", "patch"))

	got, err := buildPlan(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Modules) != 2 {
		t.Fatalf("modules = %#v, want core and craft", got.Modules)
	}
	core := got.Modules[0]
	if core.Module != "core" || core.Tag != "core/v1.2.4" {
		t.Fatalf("core plan = %#v", core)
	}
	if len(core.Deps) != 0 {
		t.Fatalf("core deps = %v, want none", core.Deps)
	}
	craft := got.Modules[1]
	if craft.Module != "craft" || craft.Current != "0.0.0" || craft.Tag != "craft/v0.0.1" {
		t.Fatalf("craft plan = %#v, want an initial craft release", craft)
	}
	if strings.Join(craft.Deps, ",") != "core" {
		t.Fatalf("craft deps = %v, want the same-batch core release", craft.Deps)
	}
	if strings.Join(got.Tags, ",") != "core/v1.2.4,craft/v0.0.1" {
		t.Fatalf("tags = %v, want dependency order", got.Tags)
	}
}

func TestPlanJSONCarriesDepsAsArray(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "craft", "require github.com/GizClaw/flowcraft/core v1.2.3\n")
	writeFile(t, repo, ".release/craft.json", validChangeset("first craft release", "craft", "minor"))
	commitAll(t, repo, "craft")

	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"plan", "--repo", repo, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("runCLI() code = %d, stderr = %s", code, stderr.String())
	}
	// The release workflow reads deps with jq; a null would abort it.
	if !strings.Contains(stdout.String(), `"deps": []`) {
		t.Fatalf("plan JSON must carry an empty deps array:\n%s", stdout.String())
	}
}

func TestBatchReplaceBlockNamesOnlySameBatchDeps(t *testing.T) {
	repo := initRepo(t)
	seedModule(t, repo, "core", "")
	seedModule(t, repo, "craft", "require github.com/GizClaw/flowcraft/core v1.2.4\n")

	plan := map[string]ModulePlan{
		"core":  {Module: "core", Dir: "core", Tag: "core/v1.2.4"},
		"craft": {Module: "craft", Dir: "craft", Tag: "craft/v0.0.1"},
	}
	if deps := sameBatchDeps("core", plan); len(deps) != 0 {
		t.Fatalf("core deps = %#v, want none", deps)
	}
	deps := sameBatchDeps("craft", plan)
	if len(deps) != 1 || deps[0].Module != "core" {
		t.Fatalf("craft deps = %#v, want core", deps)
	}

	block, paths, err := batchReplaceBlock(repo, "craft", deps)
	if err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Abs(filepath.Join(repo, "core"))
	if err != nil {
		t.Fatal(err)
	}
	want := "\nreplace (\n\tgithub.com/GizClaw/flowcraft/core => " + target + "\n)\n"
	if block != want {
		t.Fatalf("replace block = %q, want %q", block, want)
	}
	if len(paths) != 1 || !paths["github.com/GizClaw/flowcraft/core"] {
		t.Fatalf("paths = %v", paths)
	}

	original, err := os.ReadFile(filepath.Join(repo, "craft", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	// The tidied file comes back with the replacement left in it; the
	// committed go.mod must be byte-identical to the file without it.
	tidied := append(append([]byte{}, original...), []byte(block)...)
	if normalized := stripPreflightReplaces(tidied, paths); !bytes.Equal(normalized, original) {
		t.Fatalf("normalized go.mod differs:\n%s", lineDiff(original, normalized))
	}
	// go.sum cannot carry checksums for a version that is not tagged yet.
	sum := []byte("github.com/GizClaw/flowcraft/core v1.2.4 h1:abc=\n" +
		"github.com/other/mod v1.0.0 h1:def=\n")
	filtered := string(filterPreflightSums(sum, paths))
	if strings.Contains(filtered, "flowcraft/core") || !strings.Contains(filtered, "other/mod") {
		t.Fatalf("filtered go.sum = %q", filtered)
	}
	// go.sum keeps its trailing newline, so the file still matches what
	// plain `go mod tidy` writes once the replaced dependency is real.
	if !strings.HasSuffix(filtered, "def=\n") {
		t.Fatalf("filtered go.sum lost its trailing newline: %q", filtered)
	}
}

func TestPreflightTidiesAgainstSameBatchDependency(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go toolchain is required for the preflight tidy")
	}
	repo := initRepo(t)
	seedModule(t, repo, "core", "")
	seedModule(t, repo, "craft", "require github.com/GizClaw/flowcraft/core v1.2.4\n")
	commitAll(t, repo, "seed")
	gitRun(t, repo, "tag", "core/v1.2.3")

	writeFile(t, repo, "core/change.go", "package core\n")
	writeFile(t, repo, "craft/change.go",
		"package craft\n\nimport _ \"github.com/GizClaw/flowcraft/core\"\n")
	writeFile(t, repo, ".release/core.json", validChangeset("core change", "core", "patch"))
	writeFile(t, repo, ".release/craft.json", validChangeset("craft change", "craft", "patch"))

	plan, err := buildPlan(repo)
	if err != nil {
		t.Fatal(err)
	}
	byModule := make(map[string]ModulePlan, len(plan.Modules))
	for _, item := range plan.Modules {
		byModule[item.Module] = item
	}
	// core v1.2.4 does not exist yet, so the tidy only resolves through
	// the temporary local replace; GOPROXY=off proves nothing is fetched.
	t.Setenv("GOPROXY", "off")
	changed, err := preflightModule(repo, byModule["craft"], byModule, true)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatalf("craft go.mod/go.sum are not tidy against the planned core release")
	}
	mod, err := os.ReadFile(filepath.Join(repo, "craft", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mod), "replace") {
		t.Fatalf("the committed go.mod kept the preflight replace:\n%s", mod)
	}
}

func TestPreflightRequiresThePlannedDependencyPin(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go toolchain is required for the preflight tidy")
	}
	repo, byModule := sameBatchPinRepo(t, "v1.2.3")

	t.Setenv("GOPROXY", "off")
	_, err := preflightModule(repo, byModule["craft"], byModule, false)
	if err == nil {
		t.Fatal("preflight accepted a dependency left on the previous tag")
	}
	for _, want := range []string{
		"github.com/GizClaw/flowcraft/core v1.2.3 -> v1.2.4",
		"preflight --write",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("preflight error = %v, want containing %q", err, want)
		}
	}

	changed, err := preflightModule(repo, byModule["craft"], byModule, true)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("preflight --write reported no change for a stale pin")
	}
	mod, err := os.ReadFile(filepath.Join(repo, "craft", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mod), "github.com/GizClaw/flowcraft/core v1.2.4") {
		t.Fatalf("go.mod did not move to the planned version:\n%s", mod)
	}
	if strings.Contains(string(mod), "v1.2.3") || strings.Contains(string(mod), "replace") {
		t.Fatalf("go.mod kept the stale pin or the preflight replace:\n%s", mod)
	}
	// The written module is now exactly what the batch expects.
	changed, err = preflightModule(repo, byModule["craft"], byModule, true)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("preflight --write is not idempotent on the written module")
	}
}

func TestPreflightRejectsDependencyWithoutRequirement(t *testing.T) {
	repo, byModule := sameBatchPinRepo(t, "")
	_, err := preflightModule(repo, byModule["craft"], byModule, false)
	if err == nil || !strings.Contains(err.Error(), "does not require") {
		t.Fatalf("preflight error = %v, want a missing requirement error", err)
	}
}

func TestApplyDependencyPins(t *testing.T) {
	mod := []byte("module github.com/GizClaw/flowcraft/craft\n" +
		"\ngo 1.25.0\n" +
		"\nrequire (\n" +
		"\tgithub.com/GizClaw/flowcraft/core v1.2.3\n" +
		"\tgithub.com/modelcontextprotocol/go-sdk v1.7.0 // indirect\n" +
		")\n" +
		"\nrequire github.com/other/mod v0.1.0 // indirect\n")
	pins := map[string]string{"github.com/GizClaw/flowcraft/core": "v1.2.4"}

	got, moved, err := applyDependencyPins(mod, parseRequirements(mod), pins)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "\tgithub.com/GizClaw/flowcraft/core v1.2.4\n") {
		t.Fatalf("block requirement not rewritten:\n%s", got)
	}
	if strings.Contains(string(got), "v1.2.3") {
		t.Fatalf("stale version survived:\n%s", got)
	}
	if strings.Join(moved, ",") != "github.com/GizClaw/flowcraft/core v1.2.3 -> v1.2.4" {
		t.Fatalf("moved = %v", moved)
	}
	// An already-planned pin is left byte-identical.
	again, moved, err := applyDependencyPins(got, parseRequirements(got), pins)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, got) || len(moved) != 0 {
		t.Fatalf("second pass changed the file: %v\n%s", moved, lineDiff(got, again))
	}
	// A single-line indirect requirement keeps its comment.
	single := []byte("module m\n\ngo 1.25.0\n" +
		"\nrequire github.com/GizClaw/flowcraft/core v1.0.0 // indirect\n")
	got, _, err = applyDependencyPins(single, parseRequirements(single), pins)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "module m\n\ngo 1.25.0\n"+
		"\nrequire github.com/GizClaw/flowcraft/core v1.2.4 // indirect\n" {
		t.Fatalf("single-line requirement = %q", got)
	}
	// A pinned module the file does not require is an error, not an insert.
	if _, _, err := applyDependencyPins(single[:0], map[string]string{}, pins); err == nil ||
		!strings.Contains(err.Error(), "does not require") {
		t.Fatalf("missing requirement error = %v", err)
	}
}

// sameBatchPinRepo seeds a core + craft batch where craft requires core at
// pin (empty for no requirement at all) and returns the planned modules.
func sameBatchPinRepo(t *testing.T, pin string) (string, map[string]ModulePlan) {
	t.Helper()
	requirement := ""
	if pin != "" {
		requirement = "require github.com/GizClaw/flowcraft/core " + pin + "\n"
	}
	repo := initRepo(t)
	seedModule(t, repo, "core", "")
	seedModule(t, repo, "craft", requirement)
	commitAll(t, repo, "seed")
	gitRun(t, repo, "tag", "core/v1.2.3")

	writeFile(t, repo, "core/change.go", "package core\n")
	writeFile(t, repo, "craft/change.go",
		"package craft\n\nimport _ \"github.com/GizClaw/flowcraft/core\"\n")
	writeFile(t, repo, ".release/core.json", validChangeset("core change", "core", "patch"))
	writeFile(t, repo, ".release/craft.json", validChangeset("craft change", "craft", "patch"))

	plan, err := buildPlan(repo)
	if err != nil {
		t.Fatal(err)
	}
	byModule := make(map[string]ModulePlan, len(plan.Modules))
	for _, item := range plan.Modules {
		byModule[item.Module] = item
	}
	if byModule["core"].Tag != "core/v1.2.4" {
		t.Fatalf("core plan = %#v, want a v1.2.4 patch", byModule["core"])
	}
	return repo, byModule
}

func TestChangelogFillsUnreleasedPublishedStateRow(t *testing.T) {
	before := "# Changelog\n\n" +
		"## Current Published State\n\n" +
		"| Module | Latest tag | Notes |\n" +
		"| --- | --- | --- |\n" +
		"| `core` | `core/v0.4.10` | Active. |\n" +
		"| `craft` | `-` | Not released yet. |\n\n" +
		"## [Unreleased]\n"
	got, err := updatePublishedState(before, []ModulePlan{
		{Module: "core", Tag: "core/v0.4.11"},
		{Module: "craft", Tag: "craft/v0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "| `craft` | `craft/v0.0.1` | Not released yet. |") {
		t.Fatalf("craft row not filled in:\n%s", got)
	}
	if !strings.Contains(got, "| `core` | `core/v0.4.11` | Active. |") {
		t.Fatalf("core row not updated:\n%s", got)
	}

	_, err = updatePublishedState(before, []ModulePlan{{Module: "craft", Tag: "craft/v0.0.2"}})
	if err != nil {
		t.Fatalf("planned craft release without core: %v", err)
	}
}

func TestChangelogRejectsRowWithoutVersionOrPlaceholder(t *testing.T) {
	before := "# Changelog\n\n" +
		"## Current Published State\n\n" +
		"| Module | Latest tag | Notes |\n" +
		"| --- | --- | --- |\n" +
		"| `craft` | unreleased | Active. |\n\n" +
		"## [Unreleased]\n"
	if _, err := updatePublishedState(before, []ModulePlan{{Module: "craft", Tag: "craft/v0.0.1"}}); err == nil ||
		!strings.Contains(err.Error(), "no version cell") {
		t.Fatalf("updatePublishedState() error = %v, want a version cell error", err)
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
