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
	"sort"
	"strconv"
	"strings"
)

var moduleOrder = []string{"sdk", "memory", "sdkx", "voice"}

var moduleDependencies = map[string][]string{
	"memory": {"sdk"},
	"sdkx":   {"sdk", "memory"},
	"voice":  {"sdk"},
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
		fmt.Fprintln(stderr, "usage: releasegate <validate|plan> [options]")
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

	default:
		fmt.Fprintf(stderr, "unknown command %q; usage: releasegate <validate|plan> [options]\n", args[0])
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
		if strings.TrimSpace(changeset.Summary) == "" {
			return nil, fmt.Errorf("%s: summary must be non-empty", changeset.file)
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
