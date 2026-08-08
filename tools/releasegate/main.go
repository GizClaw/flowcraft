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

var moduleOrder = []string{"sdk", "memory", "sdkx"}

var moduleDependencies = map[string][]string{
	"memory": {"sdk"},
	"sdkx":   {"sdk", "memory"},
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
}

type Plan struct {
	Modules []ModulePlan `json:"modules"`
	Matrix  Matrix       `json:"matrix"`
	Tags    []string     `json:"tags"`
}

type Matrix struct {
	Include []ModulePlan `json:"include"`
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
		fmt.Fprintln(stderr, "usage: releasegate <validate|plan|changelog> [options]")
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

	default:
		fmt.Fprintf(stderr, "unknown command %q; usage: releasegate <validate|plan|changelog> [options]\n", args[0])
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
		Matrix:  Matrix{Include: make([]ModulePlan, 0)},
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
					return empty, err
				}
				latest[release.Module] = tag
			}
			consumed, err := tagContainsPath(repo, tag.tag, changeset.file)
			if err != nil {
				return empty, err
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
		changed, err := moduleChangedSince(repo, tag.tag, module)
		if err != nil {
			return empty, err
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
		}
	}

	if err := validateDependencyPins(repo, planByModule); err != nil {
		return empty, err
	}
	for _, module := range moduleOrder {
		item, ok := planByModule[module]
		if !ok {
			continue
		}
		empty.Modules = append(empty.Modules, item)
		empty.Matrix.Include = append(empty.Matrix.Include, item)
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
		if old == "" {
			return "", fmt.Errorf("Current Published State row for module %s has no version cell", module)
		}
		lines[index] = strings.Replace(line, old, "`"+tag+"`", 1)
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

func validateDependencyPins(repo string, plan map[string]ModulePlan) error {
	for _, module := range moduleOrder {
		item, pending := plan[module]
		if !pending {
			continue
		}
		requirements, err := readRequirements(filepath.Join(repo, item.Dir, "go.mod"))
		if err != nil {
			return fmt.Errorf("module %s: %w", module, err)
		}
		for _, dependency := range moduleDependencies[module] {
			dependencyPlan, sameBatch := plan[dependency]
			if !sameBatch {
				continue
			}
			modulePath, err := readModulePath(filepath.Join(repo, dependencyPlan.Dir, "go.mod"))
			if err != nil {
				return fmt.Errorf("dependency %s: %w", dependency, err)
			}
			want := "v" + dependencyPlan.Next
			if got := requirements[modulePath]; got != want {
				return fmt.Errorf("module %s must require same-batch dependency %s at %s; found %q", module, modulePath, want, got)
			}
		}
	}
	return nil
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

func readRequirements(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
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
	return requirements, nil
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
