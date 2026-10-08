package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const changelogMarker = "<!-- releasegate:releases -->"

// moduleOrder is the release-managed module list in dependency order: a
// module appears after every module it requires, so a batch can be gated
// and tagged in this order with each dependency already published.
var moduleOrder = []string{"core", "craft"}

// moduleDependencies lists the in-tree modules each release-managed
// module requires by module path. A dependency that ships in the same
// batch is tagged first and has no entry in go.sum yet, which the
// preflight tidy and the release gate both account for.
var moduleDependencies = map[string][]string{
	"craft": {"core"},
}

type Changeset struct {
	Summary  string    `json:"summary"`
	Releases []Release `json:"releases"`
	file     string
}

type Release struct {
	Module string `json:"module"`
	Bump   string `json:"bump"`
}

type ModulePlan struct {
	Module   string   `json:"module"`
	Dir      string   `json:"dir"`
	Bump     string   `json:"bump"`
	Current  string   `json:"current"`
	Next     string   `json:"next"`
	Tag      string   `json:"tag"`
	Replaces []string `json:"replaces"`
	// Deps names the modules this module requires that are tagged
	// earlier in the same batch. Their checksums are legitimately
	// absent from go.sum until the tag exists.
	Deps []string `json:"deps"`
}

type Plan struct {
	Modules []ModulePlan `json:"modules"`
	Tags    []string     `json:"tags"`
}

type version struct {
	major int
	minor int
	patch int
}

type tagVersion struct {
	tag     string
	version version
}

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: releasegate <validate|plan|changelog|preflight> [options]")
		return 2
	}

	switch args[0] {
	case "validate":
		flags := flag.NewFlagSet("validate", flag.ContinueOnError)
		flags.SetOutput(stderr)
		repo := flags.String("repo", ".", "repository root")
		base := flags.String("base", "", "base git reference")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 {
			fmt.Fprintln(stderr, "validate does not accept positional arguments")
			return 2
		}
		if err := validateRepo(*repo, *base); err != nil {
			fmt.Fprintf(stderr, "releasegate validate: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "changesets valid")
		return 0

	case "plan":
		flags := flag.NewFlagSet("plan", flag.ContinueOnError)
		flags.SetOutput(stderr)
		repo := flags.String("repo", ".", "repository root")
		jsonOutput := flags.Bool("json", false, "emit JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 {
			fmt.Fprintln(stderr, "plan does not accept positional arguments")
			return 2
		}
		plan, err := buildPlan(*repo)
		if err != nil {
			fmt.Fprintf(stderr, "releasegate plan: %v\n", err)
			return 1
		}
		if *jsonOutput {
			encoder := json.NewEncoder(stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(plan); err != nil {
				fmt.Fprintf(stderr, "releasegate plan: encode JSON: %v\n", err)
				return 1
			}
			return 0
		}
		if len(plan.Modules) == 0 {
			fmt.Fprintln(stdout, "no pending releases")
			return 0
		}
		for _, item := range plan.Modules {
			fmt.Fprintf(stdout, "%s: %s -> %s (%s), tag %s\n", item.Module, item.Current, item.Next, item.Bump, item.Tag)
		}
		return 0

	case "changelog":
		flags := flag.NewFlagSet("changelog", flag.ContinueOnError)
		flags.SetOutput(stderr)
		repo := flags.String("repo", ".", "repository root")
		check := flags.Bool("check", false, "verify CHANGELOG.md is current")
		write := flags.Bool("write", false, "write CHANGELOG.md")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 {
			fmt.Fprintln(stderr, "changelog does not accept positional arguments")
			return 2
		}
		if *check && *write {
			fmt.Fprintln(stderr, "changelog --check and --write are mutually exclusive")
			return 2
		}
		content, err := buildChangelog(*repo)
		if err != nil {
			fmt.Fprintf(stderr, "releasegate changelog: %v\n", err)
			return 1
		}
		path := filepath.Join(*repo, "CHANGELOG.md")
		if *check {
			current, err := os.ReadFile(path)
			if err != nil {
				fmt.Fprintf(stderr, "releasegate changelog: read CHANGELOG.md: %v\n", err)
				return 1
			}
			if !bytes.Equal(current, content) {
				fmt.Fprintln(stderr, "releasegate changelog: CHANGELOG.md is out of date; run changelog --write")
				return 1
			}
			return 0
		}
		if *write {
			if err := atomicWriteFile(path, content); err != nil {
				fmt.Fprintf(stderr, "releasegate changelog: %v\n", err)
				return 1
			}
			return 0
		}
		if _, err := stdout.Write(content); err != nil {
			fmt.Fprintf(stderr, "releasegate changelog: write output: %v\n", err)
			return 1
		}
		return 0

	case "preflight":
		flags := flag.NewFlagSet("preflight", flag.ContinueOnError)
		flags.SetOutput(stderr)
		repo := flags.String("repo", ".", "repository root")
		write := flags.Bool("write", false, "write tidied go.mod/go.sum instead of failing")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if flags.NArg() != 0 {
			fmt.Fprintln(stderr, "preflight does not accept positional arguments")
			return 2
		}
		if err := preflightRepo(*repo, *write, stdout); err != nil {
			fmt.Fprintf(stderr, "releasegate preflight: %v\n", err)
			return 1
		}
		return 0

	default:
		fmt.Fprintf(stderr, "unknown command %q; usage: releasegate <validate|plan|changelog|preflight> [options]\n", args[0])
		return 2
	}
}

func validateRepo(repo, base string) error {
	if _, err := loadChangesets(repo); err != nil {
		return err
	}
	if base == "" {
		return nil
	}
	output, err := git(repo, "diff", "--name-status", "--find-renames", base, "--", ".release")
	if err != nil {
		return fmt.Errorf("compare changesets with %q: %w", base, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return fmt.Errorf("parse git diff status %q", line)
		}
		path := ""
		for _, candidate := range fields[1:] {
			if filepath.Ext(candidate) == ".json" {
				path = candidate
				break
			}
		}
		if path == "" {
			continue
		}
		status := fields[0]
		if status == "A" {
			continue
		}
		if status[0] == 'D' && isLegacyChangeset(path) {
			continue
		}
		action := "changed"
		switch status[0] {
		case 'M':
			action = "modified"
		case 'D':
			action = "deleted"
		case 'R':
			action = "renamed"
		case 'C':
			action = "copied"
		}
		return fmt.Errorf("changeset %s was %s; .release changesets are immutable and may only be added", path, action)
	}
	return nil
}

func isLegacyChangeset(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "sdk") || strings.HasPrefix(base, "sdkx")
}

func preflightRepo(repo string, write bool, stdout io.Writer) error {
	plan, err := buildPlan(repo)
	if err != nil {
		return err
	}
	if len(plan.Modules) == 0 {
		fmt.Fprintln(stdout, "no pending releases")
		return nil
	}
	byModule := make(map[string]ModulePlan, len(plan.Modules))
	for _, item := range plan.Modules {
		byModule[item.Module] = item
	}
	for _, item := range plan.Modules {
		changed, err := preflightModule(repo, item, byModule, write)
		if err != nil {
			return err
		}
		if write && changed {
			fmt.Fprintf(stdout, "%s: updated go.mod/go.sum\n", item.Module)
		} else {
			fmt.Fprintf(stdout, "%s: preflight tidy OK\n", item.Module)
		}
	}
	return nil
}

func preflightModule(repo string, item ModulePlan, plan map[string]ModulePlan, write bool) (bool, error) {
	moduleDir := filepath.Join(repo, item.Dir)
	modPath := filepath.Join(moduleDir, "go.mod")
	sumPath := filepath.Join(moduleDir, "go.sum")
	modData, err := os.ReadFile(modPath)
	if err != nil {
		return false, fmt.Errorf("module %s: read go.mod: %w", item.Module, err)
	}
	sumData := []byte{}
	if _, err := os.Stat(sumPath); err == nil {
		sumData, err = os.ReadFile(sumPath)
		if err != nil {
			return false, fmt.Errorf("module %s: read go.sum: %w", item.Module, err)
		}
	}

	pins, err := sameBatchDepPins(repo, item, plan)
	if err != nil {
		return false, err
	}
	// plannedMod is go.mod as this batch requires it: the same-batch
	// dependency pins first, then the tidy result. The temporary replace
	// resolves either pin, so a dependency left on the previous tag would
	// tidy cleanly and the tag would be built against the old release.
	plannedMod, pinned, err := applyDependencyPins(modData, parseRequirements(modData), pins)
	if err != nil {
		return false, fmt.Errorf("module %s: %w", item.Module, err)
	}

	tempDir, err := os.MkdirTemp("", "releasegate-preflight-*")
	if err != nil {
		return false, fmt.Errorf("module %s: create temp dir: %w", item.Module, err)
	}
	defer os.RemoveAll(tempDir)
	tempMod := filepath.Join(tempDir, "modfile.mod")
	tempSum := filepath.Join(tempDir, "modfile.sum")
	replaceBlock, paths, err := batchReplaceBlock(repo, item.Module,
		sameBatchDeps(item.Module, plan))
	if err != nil {
		return false, err
	}
	// The replace block is appended to a copy: modData is compared with
	// the tidied file below, and appending in place could overwrite it.
	tempModData := append(append([]byte{}, plannedMod...), replaceBlock...)
	if err := os.WriteFile(tempMod, tempModData, 0o644); err != nil {
		return false, fmt.Errorf("module %s: write temp go.mod: %w", item.Module, err)
	}
	if err := os.WriteFile(tempSum, sumData, 0o644); err != nil {
		return false, fmt.Errorf("module %s: write temp go.sum: %w", item.Module, err)
	}

	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = moduleDir
	cmd.Env = withEnv(os.Environ(), map[string]string{
		"GOWORK":  "off",
		"GOFLAGS": "-modfile=" + tempMod,
	})
	if output, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("module %s: go mod tidy: %w\n%s", item.Module, err, output)
	}
	tidiedMod, err := os.ReadFile(tempMod)
	if err != nil {
		return false, fmt.Errorf("module %s: read tidied go.mod: %w", item.Module, err)
	}
	tidiedSum, err := os.ReadFile(tempSum)
	if err != nil {
		return false, fmt.Errorf("module %s: read tidied go.sum: %w", item.Module, err)
	}
	normalizedMod := stripPreflightReplaces(tidiedMod, paths)
	normalizedSum := filterPreflightSums(tidiedSum, paths)

	var changedFiles []string
	if !bytes.Equal(modData, normalizedMod) {
		changedFiles = append(changedFiles, "go.mod")
	}
	if !bytes.Equal(sumData, normalizedSum) {
		changedFiles = append(changedFiles, "go.sum")
	}
	if len(changedFiles) == 0 {
		return false, nil
	}
	if !write {
		var diff strings.Builder
		if !bytes.Equal(modData, normalizedMod) {
			fmt.Fprintf(&diff, "go.mod:\n%s", lineDiff(modData, normalizedMod))
		}
		if !bytes.Equal(sumData, normalizedSum) {
			fmt.Fprintf(&diff, "go.sum:\n%s", lineDiff(sumData, normalizedSum))
		}
		var stale strings.Builder
		if len(pinned) != 0 {
			fmt.Fprintf(&stale, "dependency pins not on the planned versions: %s\n",
				strings.Join(pinned, ", "))
		}
		return false, fmt.Errorf("module %s: %s not tidy for planned releases:\n%s%s"+
			"run `releasegate preflight --write` to apply the changes",
			item.Module, strings.Join(changedFiles, ", "), stale.String(), diff.String())
	}
	if err := writeModuleFile(modPath, normalizedMod); err != nil {
		return false, fmt.Errorf("module %s: write go.mod: %w", item.Module, err)
	}
	if err := writeModuleFile(sumPath, normalizedSum); err != nil {
		return false, fmt.Errorf("module %s: write go.sum: %w", item.Module, err)
	}
	return true, nil
}

// sameBatchDepNames lists, in moduleOrder order, the modules the given
// module requires that release in the same batch.
func sameBatchDepNames(module string, pending map[string]string) []string {
	names := make([]string, 0)
	for _, candidate := range moduleOrder {
		if candidate == module {
			continue
		}
		if !containsString(moduleDependencies[module], candidate) {
			continue
		}
		if _, ok := pending[candidate]; ok {
			names = append(names, candidate)
		}
	}
	return names
}

// sameBatchDeps returns the planned modules the given module requires,
// in moduleOrder order.
func sameBatchDeps(module string, plan map[string]ModulePlan) []ModulePlan {
	deps := make([]ModulePlan, 0, len(moduleDependencies[module]))
	for _, candidate := range moduleOrder {
		if !containsString(moduleDependencies[module], candidate) {
			continue
		}
		if item, ok := plan[candidate]; ok {
			deps = append(deps, item)
		}
	}
	return deps
}

// sameBatchDepPins maps the module paths the given module requires that
// release in the same batch to the version this batch plans to tag. The
// batch is gated and tagged in dependency order, so a dependent tagged
// after its dependency must already require the version being tagged:
// the preflight replace resolves either pin, and a requirement left on
// the previous tag would publish a release built against the old one.
func sameBatchDepPins(repo string, item ModulePlan, plan map[string]ModulePlan) (map[string]string, error) {
	deps := sameBatchDeps(item.Module, plan)
	if len(deps) == 0 {
		return nil, nil
	}
	pins := make(map[string]string, len(deps))
	for _, dep := range deps {
		modulePath, err := readModulePath(filepath.Join(repo, dep.Dir, "go.mod"))
		if err != nil {
			return nil, fmt.Errorf("module %s: %w", item.Module, err)
		}
		pins[modulePath] = "v" + dep.Next
	}
	return pins, nil
}

// applyDependencyPins rewrites each pinned requirement to the version the
// batch plans and reports the pins it moved. A pinned module path the
// file does not require is an error: requirement versions are rewritten,
// never invented.
func applyDependencyPins(
	modData []byte,
	requirements map[string]string,
	pins map[string]string,
) ([]byte, []string, error) {
	if len(pins) == 0 {
		return modData, nil, nil
	}
	paths := make([]string, 0, len(pins))
	for modulePath := range pins {
		paths = append(paths, modulePath)
	}
	sort.Strings(paths)

	updated := string(modData)
	var moved []string
	for _, modulePath := range paths {
		expected := pins[modulePath]
		current, ok := requirements[modulePath]
		if !ok {
			return nil, nil, fmt.Errorf(
				"go.mod does not require %s; add the requirement to the release PR",
				modulePath)
		}
		if current == expected {
			continue
		}
		// Rewrite the version token that follows the module path: the
		// line keeps its indentation and any trailing comment.
		needle := modulePath + " " + current
		index := strings.Index(updated, needle)
		if index < 0 {
			return nil, nil, fmt.Errorf(
				"go.mod: cannot rewrite the requirement for %s", modulePath)
		}
		updated = updated[:index] + modulePath + " " + expected +
			updated[index+len(needle):]
		moved = append(moved, modulePath+" "+current+" -> "+expected)
	}
	if len(moved) == 0 {
		return modData, nil, nil
	}
	return []byte(updated), moved, nil
}

// batchReplaceBlock renders the temporary `replace` block that points
// same-batch dependencies at their local directories, so the preflight
// tidy resolves the versions this batch is about to tag. It returns the
// module paths it replaced, which the caller strips from the tidied
// go.mod/go.sum: the committed files must stay replace-free and carry no
// checksums for a version that does not exist yet.
func batchReplaceBlock(repo, module string, deps []ModulePlan) (string, map[string]bool, error) {
	paths := make(map[string]bool, len(deps))
	if len(deps) == 0 {
		return "", paths, nil
	}
	var replace strings.Builder
	replace.WriteString("\nreplace (\n")
	for _, dep := range deps {
		depDir := filepath.Join(repo, dep.Dir)
		modulePath, err := readModulePath(filepath.Join(depDir, "go.mod"))
		if err != nil {
			return "", nil, err
		}
		target, err := filepath.Abs(depDir)
		if err != nil {
			return "", nil, fmt.Errorf("module %s: resolve %s path: %w", module, dep.Module, err)
		}
		paths[modulePath] = true
		fmt.Fprintf(&replace, "\t%s => %s\n", modulePath, target)
	}
	replace.WriteString(")\n")
	return replace.String(), paths, nil
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func readModulePath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(stripComment(line))
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("%s has no module directive", path)
}

func stripPreflightReplaces(content []byte, paths map[string]bool) []byte {
	if len(paths) == 0 {
		return content
	}
	lines := strings.Split(string(content), "\n")
	var out []string
	var block []string
	inBlock := false
	flushBlock := func() {
		kept := make([]string, 0, len(block))
		for _, line := range block {
			if !replaceLineTargets(line, paths) {
				kept = append(kept, line)
			}
		}
		// A block whose replacements were all stripped keeps only its
		// delimiters: an empty `replace (...)` directive is not what
		// plain `go mod tidy` writes, so drop it whole.
		if hasReplaceEntry(kept) {
			out = append(out, kept...)
		}
		block = nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inBlock && strings.HasPrefix(trimmed, "replace (") {
			inBlock = true
			block = []string{line}
			continue
		}
		if inBlock {
			block = append(block, line)
			if trimmed == ")" {
				flushBlock()
				inBlock = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "replace ") && !strings.HasPrefix(trimmed, "replace (") &&
			replaceLineTargets(line, paths) {
			continue
		}
		out = append(out, line)
	}
	// The preflight replace block is appended after a leading newline, so
	// stripping it leaves a blank line that plain `go mod tidy` would not
	// write. Drop the extra trailing empty lines, keeping the one that
	// represents the final newline, so --write output matches tidy.
	for len(out) > 1 && out[len(out)-1] == "" && out[len(out)-2] == "" {
		out = out[:len(out)-1]
	}
	return []byte(strings.Join(out, "\n"))
}

func replaceLineTargets(line string, paths map[string]bool) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	if fields[0] == "replace" {
		return len(fields) >= 2 && paths[fields[1]]
	}
	return paths[fields[0]]
}

// hasReplaceEntry reports whether a replace block kept any line other
// than its own delimiters.
func hasReplaceEntry(lines []string) bool {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "replace (" || trimmed == ")" {
			continue
		}
		return true
	}
	return false
}

func filterPreflightSums(sum []byte, paths map[string]bool) []byte {
	if len(paths) == 0 {
		return sum
	}
	lines := strings.Split(string(sum), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 1 && paths[fields[0]] {
			continue
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return nil
	}
	// go.sum ends in a newline; keep exactly one so the committed file
	// matches what `go mod tidy` writes without the replace block.
	return []byte(strings.Join(out, "\n") + "\n")
}

func lineDiff(oldData, newData []byte) string {
	oldLines := splitLines(oldData)
	newLines := splitLines(newData)
	rows, cols := len(oldLines)+1, len(newLines)+1
	dp := make([][]int, rows)
	for i := range dp {
		dp[i] = make([]int, cols)
	}
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var diff strings.Builder
	i, j := 0, 0
	for i < len(oldLines) && j < len(newLines) {
		if oldLines[i] == newLines[j] {
			fmt.Fprintf(&diff, "  %s\n", oldLines[i])
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			fmt.Fprintf(&diff, "- %s\n", oldLines[i])
			i++
		} else {
			fmt.Fprintf(&diff, "+ %s\n", newLines[j])
			j++
		}
	}
	for ; i < len(oldLines); i++ {
		fmt.Fprintf(&diff, "- %s\n", oldLines[i])
	}
	for ; j < len(newLines); j++ {
		fmt.Fprintf(&diff, "+ %s\n", newLines[j])
	}
	return diff.String()
}

func splitLines(data []byte) []string {
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func withEnv(env []string, overrides map[string]string) []string {
	var out []string
	for _, entry := range env {
		key := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			key = entry[:index]
		}
		if _, ok := overrides[key]; !ok {
			out = append(out, entry)
		}
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}

func writeModuleFile(path string, content []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, content, mode)
}

func loadChangesets(repo string) ([]Changeset, error) {
	matches, err := filepath.Glob(filepath.Join(repo, ".release", "*.json"))
	if err != nil {
		return nil, fmt.Errorf("find changesets: %w", err)
	}
	sort.Strings(matches)
	changesets := make([]Changeset, 0, len(matches))
	validModules := make(map[string]bool, len(moduleOrder))
	for _, module := range moduleOrder {
		validModules[module] = true
	}

	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var changeset Changeset
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&changeset); err != nil {
			return nil, fmt.Errorf("%s: invalid JSON: %w", relativePath(repo, path), err)
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return nil, fmt.Errorf("%s: invalid JSON: %w", relativePath(repo, path), err)
		}
		changeset.file = relativePath(repo, path)
		changeset.Summary = strings.TrimSpace(changeset.Summary)
		if changeset.Summary == "" {
			return nil, fmt.Errorf("%s: summary must be non-empty", changeset.file)
		}
		if strings.ContainsAny(changeset.Summary, "\r\n") {
			return nil, fmt.Errorf("%s: summary must be a single line", changeset.file)
		}
		if strings.Contains(changeset.Summary, changelogMarker) {
			return nil, fmt.Errorf("%s: summary contains reserved changelog marker", changeset.file)
		}
		if len(changeset.Releases) == 0 {
			return nil, fmt.Errorf("%s: releases must contain at least one release", changeset.file)
		}
		seen := make(map[string]bool)
		for _, release := range changeset.Releases {
			if !validModules[release.Module] {
				return nil, fmt.Errorf("%s: invalid module %q", changeset.file, release.Module)
			}
			if release.Bump != "patch" && release.Bump != "minor" {
				return nil, fmt.Errorf("%s: invalid bump %q for module %s", changeset.file, release.Bump, release.Module)
			}
			if seen[release.Module] {
				return nil, fmt.Errorf("%s: duplicate release declaration for module %s", changeset.file, release.Module)
			}
			seen[release.Module] = true
		}
		changesets = append(changesets, changeset)
	}
	return changesets, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values")
}

func buildPlan(repo string) (Plan, error) {
	empty := Plan{
		Modules: make([]ModulePlan, 0),
		Tags:    make([]string, 0),
	}
	changesets, err := loadChangesets(repo)
	if err != nil {
		return empty, err
	}
	if len(changesets) == 0 {
		return empty, nil
	}

	latest := make(map[string]tagVersion)
	pendingBumps := make(map[string]string)
	pendingFiles := make(map[string][]string)
	for _, changeset := range changesets {
		for _, release := range changeset.Releases {
			tag, ok := latest[release.Module]
			if !ok {
				tag, err = latestModuleTag(repo, release.Module)
				if err != nil {
					if !strings.Contains(err.Error(), "no seed tag") {
						return empty, err
					}
					tag = tagVersion{}
				}
				latest[release.Module] = tag
			}
			consumed := false
			if tag.tag != "" {
				consumed, err = tagContainsPath(repo, tag.tag, changeset.file)
				if err != nil {
					return empty, err
				}
			}
			if consumed {
				continue
			}
			pendingBumps[release.Module] = higherBump(pendingBumps[release.Module], release.Bump)
			pendingFiles[release.Module] = append(pendingFiles[release.Module], changeset.file)
		}
	}

	if len(pendingBumps) == 0 {
		return empty, nil
	}

	planByModule := make(map[string]ModulePlan)
	for _, module := range moduleOrder {
		bump, ok := pendingBumps[module]
		if !ok {
			continue
		}
		tag := latest[module]
		changed := true
		if tag.tag != "" {
			changed, err = moduleChangedSince(repo, tag.tag, module)
			if err != nil {
				return empty, err
			}
		}
		if !changed {
			return empty, fmt.Errorf("module %s has pending changesets but no changes since %s", module, tag.tag)
		}
		next := bumpVersion(tag.version, bump)
		replaces := pendingFiles[module]
		sort.Strings(replaces)
		planByModule[module] = ModulePlan{
			Module:   module,
			Dir:      module,
			Bump:     bump,
			Current:  tag.version.String(),
			Next:     next.String(),
			Tag:      module + "/v" + next.String(),
			Replaces: replaces,
			Deps:     sameBatchDepNames(module, pendingBumps),
		}
	}

	for _, module := range moduleOrder {
		item, ok := planByModule[module]
		if !ok {
			continue
		}
		empty.Modules = append(empty.Modules, item)
		empty.Tags = append(empty.Tags, item.Tag)
	}
	return empty, nil
}

func buildChangelog(repo string) ([]byte, error) {
	plan, err := buildPlan(repo)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(repo, "CHANGELOG.md")
	current, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CHANGELOG.md: %w", err)
	}
	if len(plan.Modules) == 0 {
		return current, nil
	}

	markerCount := bytes.Count(current, []byte(changelogMarker))
	if markerCount != 1 {
		return nil, fmt.Errorf("CHANGELOG.md must contain exactly one %s marker; found %d", changelogMarker, markerCount)
	}
	changesets, err := loadChangesets(repo)
	if err != nil {
		return nil, err
	}
	byFile := make(map[string]Changeset, len(changesets))
	for _, changeset := range changesets {
		byFile[changeset.file] = changeset
	}

	content := string(current)
	content, err = removeUnpublishedSections(repo, content, plan.Modules)
	if err != nil {
		return nil, err
	}
	var additions []string
	for _, item := range plan.Modules {
		date, summaries, err := changelogRelease(repo, item, byFile)
		if err != nil {
			return nil, err
		}
		section := formatReleaseSection(item.Tag, date, summaries)
		additions = append(additions, section)
	}

	content, err = updatePublishedState(content, plan.Modules)
	if err != nil {
		return nil, err
	}
	if len(additions) != 0 {
		index := strings.Index(content, changelogMarker) + len(changelogMarker)
		insertion := "\n\n" + strings.Join(additions, "\n\n")
		content = content[:index] + insertion + content[index:]
	}
	return []byte(content), nil
}

type changelogSectionSpan struct {
	start int
	end   int
	tag   string
}

func removeUnpublishedSections(repo, changelog string, modules []ModulePlan) (string, error) {
	pending := make(map[string]bool, len(modules))
	for _, item := range modules {
		pending[item.Module] = true
	}
	output, err := git(repo, "tag", "--list")
	if err != nil {
		return "", fmt.Errorf("list tags: %w", err)
	}
	existingTags := make(map[string]bool)
	for _, tag := range strings.Fields(output) {
		existingTags[tag] = true
	}

	var headings []int
	for offset := 0; offset < len(changelog); {
		end := strings.IndexByte(changelog[offset:], '\n')
		if end < 0 {
			end = len(changelog) - offset
		}
		line := changelog[offset : offset+end]
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, offset)
		}
		offset += end + 1
	}

	var candidates []changelogSectionSpan
	for index, start := range headings {
		lineEnd := strings.IndexByte(changelog[start:], '\n')
		if lineEnd < 0 {
			lineEnd = len(changelog) - start
		}
		tag, module, ok := releaseHeading(changelog[start : start+lineEnd])
		if !ok || !pending[module] {
			continue
		}
		end := len(changelog)
		if index+1 < len(headings) {
			end = headings[index+1]
		}
		candidates = append(candidates, changelogSectionSpan{start: start, end: end, tag: tag})
	}

	var removals []changelogSectionSpan
	for _, candidate := range candidates {
		if !existingTags[candidate.tag] {
			removals = append(removals, candidate)
		}
	}
	for index := len(removals) - 1; index >= 0; index-- {
		span := removals[index]
		changelog = changelog[:span.start] + changelog[span.end:]
	}
	return changelog, nil
}

func releaseHeading(line string) (tag, module string, ok bool) {
	const prefix = "## `"
	const separator = "` - "
	if !strings.HasPrefix(line, prefix) {
		return "", "", false
	}
	end := strings.Index(line[len(prefix):], separator)
	if end < 0 {
		return "", "", false
	}
	tag = line[len(prefix) : len(prefix)+end]
	slash := strings.Index(tag, "/v")
	if slash <= 0 {
		return "", "", false
	}
	return tag, tag[:slash], true
}

func changelogRelease(repo string, item ModulePlan, changesets map[string]Changeset) (string, []string, error) {
	var latestDate string
	var summaries []string
	seen := make(map[string]bool)
	for _, file := range item.Replaces {
		changeset, ok := changesets[file]
		if !ok {
			return "", nil, fmt.Errorf("module %s references missing changeset %s", item.Module, file)
		}
		output, err := git(repo, "log", "-1", "--format=%cs", "--", file)
		if err != nil {
			return "", nil, fmt.Errorf("find commit date for %s: %w", file, err)
		}
		date := strings.TrimSpace(output)
		if date == "" {
			return "", nil, fmt.Errorf("%s has no commit date", file)
		}
		if date > latestDate {
			latestDate = date
		}
		belongs := false
		for _, release := range changeset.Releases {
			if release.Module == item.Module {
				belongs = true
				break
			}
		}
		if belongs && !seen[changeset.Summary] {
			seen[changeset.Summary] = true
			summaries = append(summaries, changeset.Summary)
		}
	}
	return latestDate, summaries, nil
}

func formatReleaseSection(tag, date string, summaries []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "## `%s` - %s\n\n### Changed\n\n", tag, date)
	for _, summary := range summaries {
		fmt.Fprintf(&builder, "- %s\n", summary)
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

// unreleasedCell marks the version cell of a module in the published
// state table that has no tag yet.
const unreleasedCell = "-"

// isUnreleasedCell reports whether a published-state version cell holds
// the placeholder for a module that has not been tagged yet.
func isUnreleasedCell(cell string) bool {
	return strings.Trim(strings.TrimSpace(cell), "`") == unreleasedCell
}

// updatePublishedState rewrites the "Current Published State" row of
// every planned module to its new tag. A row whose version cell is
// unreleasedCell is filled in by the module's first release.
func updatePublishedState(changelog string, modules []ModulePlan) (string, error) {
	tags := make(map[string]string, len(modules))
	for _, item := range modules {
		tags[item.Module] = item.Tag
	}
	lines := strings.Split(changelog, "\n")
	tableStart := -1
	tableEnd := len(lines)
	for index, line := range lines {
		if line == "## Current Published State" {
			tableStart = index + 1
			continue
		}
		if tableStart >= 0 && strings.HasPrefix(line, "## ") {
			tableEnd = index
			break
		}
	}
	if tableStart < 0 {
		return "", errors.New("CHANGELOG.md has no Current Published State section")
	}
	updated := make(map[string]bool, len(tags))
	for index := tableStart; index < tableEnd; index++ {
		line := lines[index]
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		module := strings.Trim(strings.TrimSpace(cells[1]), "`")
		tag, ok := tags[module]
		if !ok {
			continue
		}
		oldRe := regexp.MustCompile("`" + regexp.QuoteMeta(module) +
			"/v[0-9]+\\.[0-9]+\\.[0-9]+`")
		old := oldRe.FindString(cells[2])
		switch {
		case old != "":
			lines[index] = strings.Replace(line, old, "`"+tag+"`", 1)
		case isUnreleasedCell(cells[2]):
			cells[2] = " `" + tag + "` "
			lines[index] = strings.Join(cells, "|")
		default:
			return "", fmt.Errorf("Current Published State row for module %s has no version cell", module)
		}
		updated[module] = true
	}
	for module := range tags {
		if !updated[module] {
			return "", fmt.Errorf("Current Published State has no row for module %s", module)
		}
	}
	return strings.Join(lines, "\n"), nil
}

func atomicWriteFile(path string, content []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".releasegate-changelog-*")
	if err != nil {
		return fmt.Errorf("create temporary changelog: %w", err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(info.Mode().Perm()); err != nil {
		file.Close()
		return fmt.Errorf("set temporary changelog permissions: %w", err)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return fmt.Errorf("write temporary changelog: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync temporary changelog: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary changelog: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace CHANGELOG.md: %w", err)
	}
	return nil
}

func latestModuleTag(repo, module string) (tagVersion, error) {
	output, err := git(repo, "tag", "--list", module+"/v*")
	if err != nil {
		return tagVersion{}, fmt.Errorf("list tags for module %s: %w", module, err)
	}
	var latest tagVersion
	found := false
	for _, tag := range strings.Fields(output) {
		value, ok := parseVersion(strings.TrimPrefix(tag, module+"/v"))
		if !ok {
			continue
		}
		if !found || compareVersions(value, latest.version) > 0 {
			latest = tagVersion{tag: tag, version: value}
			found = true
		}
	}
	if !found {
		return tagVersion{}, fmt.Errorf("module %s has no seed tag matching %s/vX.Y.Z", module, module)
	}
	return latest, nil
}

func tagContainsPath(repo, tag, path string) (bool, error) {
	output, err := git(repo, "ls-tree", "--name-only", tag, "--", path)
	if err != nil {
		return false, fmt.Errorf("inspect %s for %s: %w", tag, path, err)
	}
	return strings.TrimSpace(output) == path, nil
}

func moduleChangedSince(repo, tag, module string) (bool, error) {
	cmd := exec.Command("git", "-C", repo, "diff", "--quiet", tag, "--", module)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return true, nil
		}
		return false, fmt.Errorf("compare module %s with %s: %w", module, tag, err)
	}
	untracked, err := git(repo, "ls-files", "--others", "--exclude-standard", "--", module)
	if err != nil {
		return false, fmt.Errorf("find untracked changes for module %s: %w", module, err)
	}
	return strings.TrimSpace(untracked) != "", nil
}

// parseRequirements maps the module paths a go.mod file requires to their
// versions; both the single-line and the block form are read.
func parseRequirements(data []byte) map[string]string {
	requirements := make(map[string]string)
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(stripComment(line))
		if len(fields) == 0 {
			continue
		}
		if inBlock {
			if fields[0] == ")" {
				inBlock = false
				continue
			}
			if len(fields) >= 2 {
				requirements[fields[0]] = fields[1]
			}
			continue
		}
		if fields[0] != "require" {
			continue
		}
		if len(fields) == 2 && fields[1] == "(" {
			inBlock = true
		} else if len(fields) >= 3 {
			requirements[fields[1]] = fields[2]
		}
	}
	return requirements
}

func stripComment(line string) string {
	if index := strings.Index(line, "//"); index >= 0 {
		return line[:index]
	}
	return line
}

func higherBump(current, candidate string) string {
	if current == "minor" || candidate == "minor" {
		return "minor"
	}
	return candidate
}

func bumpVersion(current version, bump string) version {
	if bump == "minor" {
		return version{major: current.major, minor: current.minor + 1}
	}
	return version{major: current.major, minor: current.minor, patch: current.patch + 1}
}

func parseVersion(value string) (version, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	numbers := make([]int, 3)
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return version{}, false
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return version{}, false
		}
		numbers[index] = number
	}
	return version{major: numbers[0], minor: numbers[1], patch: numbers[2]}, true
}

func compareVersions(left, right version) int {
	if left.major != right.major {
		return left.major - right.major
	}
	if left.minor != right.minor {
		return left.minor - right.minor
	}
	return left.patch - right.patch
}

func (v version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

func relativePath(repo, path string) string {
	relative, err := filepath.Rel(repo, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

func git(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, message)
	}
	return string(output), nil
}
