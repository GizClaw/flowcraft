// Command coveragegate is the coverage ratchet for the core module.
//
// A single total is the wrong number to gate on: it moves by two points when a
// well-tested file is split, and it hides the package that lost its tests behind
// the package that gained some. This tool keeps a per-package floor instead, the
// way .golangci.yml keeps a complexity baseline — a floor is what a package
// reached when it was last touched, it may only go up, and a drop fails with the
// package, the two numbers and the delta.
//
// # Why the profile is post-processed
//
// `go test -coverpkg=./...` merges one profile per test binary, so the same block
// appears once per binary that linked it. Summing the raw file therefore counts a
// package as many times as it has consumers; the tool keys blocks by position and
// keeps the highest count, which is the deduplication `go tool cover` does before
// it reports a total.
//
// # What a floor may be
//
// A number is that package's statement coverage, rounded down to a whole percent
// when written, so ordinary churn does not force an edit. "-" marks a package with
// no floor: the conformance suites are executed by the modules that own an
// implementation, not by core, so a core-only profile reports nothing for them and
// a floor there would be a claim nobody made.
//
// A package measuring 0.0% cannot be written automatically. That is either a
// conformance suite, which a human marks "-", or a package whose tests are gone,
// which is the failure this gate exists for.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "coveragegate: %v\n", err)
		os.Exit(1)
	}
}

// noFloor is the marker for a package that carries no floor.
const noFloor = "-"

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("coveragegate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "", "coverage profile written by go test -coverprofile")
	floorPath := fs.String("floor", "", "per-package floor file")
	prefix := fs.String("prefix", "github.com/GizClaw/flowcraft/core", "import path prefix stripped to name a package")
	write := fs.Bool("write", false, "rewrite the floor file from this profile instead of checking it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *profile == "" || *floorPath == "" {
		return fmt.Errorf("usage: coveragegate -profile <cover.out> -floor <coverage-floor.txt> [-write]")
	}

	measured, err := readProfile(*profile, *prefix)
	if err != nil {
		return err
	}
	if len(measured) == 0 {
		return fmt.Errorf("%s: no package was measured; a gate that reads nothing passes on nothing", *profile)
	}

	floors, err := readFloors(*floorPath)
	if err != nil {
		return err
	}
	if *write {
		return writeFloors(*floorPath, measured, floors, stdout)
	}
	return checkFloors(measured, floors, *floorPath, stdout)
}

// Package is one package's measured coverage.
type Package struct {
	// Name is the package path with the module prefix removed ("." for the root).
	Name string
	// Statements is how many statements the package has.
	Statements int
	// Covered is how many of them a test executed.
	Covered int
}

// Percent is the package's statement coverage.
func (p Package) Percent() float64 {
	if p.Statements == 0 {
		return 0
	}
	return 100 * float64(p.Covered) / float64(p.Statements)
}

// block is one entry of the profile, keyed by its position so the merge of several
// test binaries counts it once.
type block struct {
	pkg   string
	stmts int
	count int
}

// readProfile aggregates a coverage profile per package.
func readProfile(path, prefix string) (map[string]Package, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	blocks := map[string]block{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		// <file>:<startLine>.<startCol>,<endLine>.<endCol> <numStmt> <count>
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s: malformed profile line %q", path, line)
		}
		position := fields[0]
		stmts, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("%s: statement count in %q: %w", path, line, err)
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("%s: execution count in %q: %w", path, line, err)
		}
		separator := strings.LastIndex(position, ":")
		if separator < 0 {
			return nil, fmt.Errorf("%s: no position separator in %q", path, line)
		}
		name := packageName(position[:separator], prefix)
		existing, seen := blocks[position]
		if seen && existing.count >= count {
			continue
		}
		blocks[position] = block{pkg: name, stmts: stmts, count: count}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	measured := map[string]Package{}
	for _, b := range blocks {
		pkg := measured[b.pkg]
		pkg.Name = b.pkg
		pkg.Statements += b.stmts
		if b.count > 0 {
			pkg.Covered += b.stmts
		}
		measured[b.pkg] = pkg
	}
	return measured, nil
}

// packageName turns a file path in the profile into its package's name relative to
// the module.
func packageName(file, prefix string) string {
	if !strings.HasPrefix(file, prefix) {
		return file
	}
	rest := strings.TrimPrefix(file, prefix)
	rest = strings.TrimPrefix(rest, "/")
	if slash := strings.LastIndex(rest, "/"); slash >= 0 {
		return rest[:slash]
	}
	if rest == "" {
		return "."
	}
	// A file at the module root: only the file name is left, and the package is
	// the root.
	if !strings.Contains(rest, "/") {
		return "."
	}
	return rest
}

// readFloors parses the floor file. A floor of "-" is kept as NaN-free marker via the
// returned map's presence with value noFloorValue.
func readFloors(path string) (map[string]float64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	floors := map[string]float64{}
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: want \"<package> <floor>\", got %q", path, line, text)
		}
		if fields[1] == noFloor {
			floors[fields[0]] = noFloorValue
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: floor %q: %w", path, line, fields[1], err)
		}
		if value < 0 || value > 100 {
			return nil, fmt.Errorf("%s:%d: floor %v is not a percentage", path, line, value)
		}
		floors[fields[0]] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(floors) == 0 {
		return nil, fmt.Errorf("%s: no floors; an empty file makes every package unmeasured", path)
	}
	return floors, nil
}

// noFloorValue is how "-" travels through the map: a sentinel no measurement can
// equal, which keeps the two cases distinguishable without a second map.
const noFloorValue = -1

// checkFloors compares the profile with the floors and reports every difference.
func checkFloors(measured map[string]Package, floors map[string]float64, floorPath string, stdout io.Writer) error {
	names := make([]string, 0, len(measured))
	for name := range measured {
		names = append(names, name)
	}
	sort.Strings(names)

	var failures []string
	for _, name := range names {
		pkg := measured[name]
		floor, ok := floors[name]
		if !ok {
			failures = append(failures, fmt.Sprintf(
				"%s: no floor (%d/%d statements, %.1f%%); run `make cover-write` and commit the result",
				name, pkg.Covered, pkg.Statements, pkg.Percent()))
			continue
		}
		if floor == noFloorValue {
			fmt.Fprintf(stdout, "  %-34s %6.1f%%  (no floor)\n", name, pkg.Percent())
			continue
		}
		fmt.Fprintf(stdout, "  %-34s %6.1f%%  floor %5.1f%%\n", name, pkg.Percent(), floor)
		if pkg.Percent() < floor {
			failures = append(failures, fmt.Sprintf(
				"%s: %.1f%% is below its floor of %.1f%% (%d/%d statements); "+
					"add the test back, or lower the floor in %s and say why in the review",
				name, pkg.Percent(), floor, pkg.Covered, pkg.Statements, floorPath))
		}
	}

	// A floor for a package that is no longer measured is a floor nothing enforces:
	// it hides both a deleted package and a renamed one.
	stale := make([]string, 0, len(floors))
	for name := range floors {
		if _, ok := measured[name]; !ok {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		failures = append(failures, fmt.Sprintf(
			"%s: floor listed in %s but not measured by the profile; remove it or restore the package",
			name, floorPath))
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("%d coverage failure(s):\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return nil
}

// writeFloors rewrites the floor file from a profile.
//
// A floor is rounded down, and a package that is not in the old file is added. A
// package measuring 0.0% is refused: it is either a conformance suite, which a human
// marks "-", or a package whose tests are gone — and the second one is the failure
// this gate exists for.
func writeFloors(path string, measured map[string]Package, previous map[string]float64, stdout io.Writer) error {
	names := make([]string, 0, len(measured))
	for name := range measured {
		names = append(names, name)
	}
	sort.Strings(names)

	var out strings.Builder
	out.WriteString("# Statement-coverage floors for core, one per package.\n")
	out.WriteString("#\n")
	out.WriteString("# A floor is what the package reached when it was last touched, rounded down to a\n")
	out.WriteString("# whole percent so ordinary churn does not force an edit. It may only go up: a\n")
	out.WriteString("# drop fails `make cover-check`, and lowering a number is a claim the reviewer of\n")
	out.WriteString("# the pull request has to accept.\n")
	out.WriteString("#\n")
	out.WriteString("# \"-\" marks a package with no floor. The conformance suites are executed by the\n")
	out.WriteString("# modules that own an implementation, not by core, so a core-only profile reports\n")
	out.WriteString("# nothing for them and a floor there would be a claim nobody made.\n")
	out.WriteString("#\n")
	out.WriteString("# Regenerate with `make cover-write`; the suite entries below are preserved by hand.\n")
	out.WriteString("#\n")
	out.WriteString("# package                            floor\n")
	for _, name := range names {
		pkg := measured[name]
		if pkg.Percent() == 0 {
			if previous[name] == noFloorValue {
				fmt.Fprintf(&out, "%-34s %s\n", name, noFloor)
				continue
			}
			return fmt.Errorf("%s measures 0.0%%: either it is a conformance suite (mark it %q in %s) "+
				"or its tests are gone, which is the regression this gate exists for",
				name, noFloor, path)
		}
		// Rounded down, not to nearest: a floor is what the package reached, and
		// rounding 82.9 up to 83 fails the very next run that reads the same code.
		fmt.Fprintf(&out, "%-34s %.0f\n", name, math.Floor(pkg.Percent()))
		fmt.Fprintf(stdout, "  %-34s %6.1f%%\n", name, pkg.Percent())
	}
	if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
		return err
	}
	return nil
}
