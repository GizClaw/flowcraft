package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// profile writes a coverage profile and returns its path.
func profile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cover.out")
	body := "mode: set\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	return path
}

// blockLine renders one profile line for the module prefix the tool defaults to.
func blockLine(file, position string, stmts, count int) string {
	return "github.com/GizClaw/flowcraft/core/" + file + ":" + position + " " +
		strconv.Itoa(stmts) + " " + strconv.Itoa(count)
}

func floors(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coverage-floor.txt")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write floors: %v", err)
	}
	return path
}

// TestReadProfileDedupesAcrossTestBinaries is the reason the tool post-processes the
// profile at all: `-coverpkg=./...` writes one entry per test binary that linked the
// block, and summing them reports a package as many times as it has consumers.
func TestReadProfileDedupesAcrossTestBinaries(t *testing.T) {
	file := profile(t,
		// The same block, seen by two test binaries, with different counts.
		blockLine("host/session.go", "10.1,12.2", 4, 0),
		blockLine("host/session.go", "10.1,12.2", 4, 7),
		blockLine("host/session.go", "20.1,22.2", 6, 0),
	)

	measured, err := readProfile(file, "github.com/GizClaw/flowcraft/core")
	if err != nil {
		t.Fatalf("readProfile: %v", err)
	}
	got, ok := measured["host"]
	if !ok {
		t.Fatalf("measured packages = %v, want host", measured)
	}
	if got.Statements != 10 || got.Covered != 4 {
		t.Fatalf("host = %d/%d, want 4/10", got.Covered, got.Statements)
	}
	if got.Percent() != 40 {
		t.Fatalf("host = %v%%, want 40", got.Percent())
	}
}

// TestPackageName covers the two shapes a profile path takes: a file in a package and a
// file at the module root, whose name is all that is left after the prefix.
func TestPackageName(t *testing.T) {
	const prefix = "github.com/GizClaw/flowcraft/core"
	for _, tc := range []struct{ file, want string }{
		{prefix + "/host/session.go", "host"},
		{prefix + "/telemetry/setup/trace.go", "telemetry/setup"},
		{prefix + "/doc.go", "."},
		{"github.com/elsewhere/pkg/file.go", "github.com/elsewhere/pkg/file.go"},
	} {
		if got := packageName(tc.file, prefix); got != tc.want {
			t.Errorf("packageName(%q) = %q, want %q", tc.file, got, tc.want)
		}
	}
}

// TestCheckFloorsReportsADrop is the gate itself: a package under its floor fails, and
// the message carries the two numbers a reviewer needs.
func TestCheckFloorsReportsADrop(t *testing.T) {
	measured := map[string]Package{
		"host": {Name: "host", Statements: 100, Covered: 80},
	}
	path := floors(t, "host 86\n")

	err := checkFloors(measured, map[string]float64{"host": 86}, path, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a package below its floor passed")
	}
	for _, want := range []string{"host", "80.0%", "86.0%", "100 statements"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestCheckFloorsReportsAMissingFloor(t *testing.T) {
	measured := map[string]Package{
		"host": {Name: "host", Statements: 10, Covered: 10},
	}
	path := floors(t, "tool 80\n")

	err := checkFloors(measured, map[string]float64{"tool": 80}, path, &bytes.Buffer{})
	if err == nil {
		t.Fatal("an unregistered package passed")
	}
	if !strings.Contains(err.Error(), "make cover-write") {
		t.Errorf("error %q does not say how to fix it", err)
	}
}

// TestCheckFloorsReportsAStaleFloor keeps a deleted package from leaving a floor that
// nothing enforces, which is the same rule layout.Table applies to its own entries.
func TestCheckFloorsReportsAStaleFloor(t *testing.T) {
	measured := map[string]Package{
		"host": {Name: "host", Statements: 10, Covered: 5},
	}
	path := floors(t, "host 50\nagent 80\n")

	err := checkFloors(measured, map[string]float64{"host": 50, "agent": 80}, path, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a stale floor passed")
	}
	if !strings.Contains(err.Error(), "agent") {
		t.Errorf("error %q does not name the stale package", err)
	}
}

// TestCheckFloorsSkipsASuite: the conformance suites are executed by the modules that
// own an implementation, so a core-only profile reports nothing for them.
func TestCheckFloorsSkipsASuite(t *testing.T) {
	measured := map[string]Package{
		"sandbox/sandboxtest": {Name: "sandbox/sandboxtest", Statements: 300, Covered: 0},
	}
	path := floors(t, "sandbox/sandboxtest -\n")

	var out bytes.Buffer
	if err := checkFloors(measured, map[string]float64{"sandbox/sandboxtest": noFloorValue}, path, &out); err != nil {
		t.Fatalf("a suite entry failed: %v", err)
	}
	if !strings.Contains(out.String(), "no floor") {
		t.Errorf("output %q does not show the suite was skipped", out.String())
	}
}

// TestCheckFloorsAcceptsARaisedMeasurement: a floor is a floor, not a target. A package
// that gained coverage passes and says so, which is what makes `make cover-write` a
// ratchet a developer wants to run.
func TestCheckFloorsAcceptsARaisedMeasurement(t *testing.T) {
	measured := map[string]Package{
		"host": {Name: "host", Statements: 100, Covered: 90},
	}
	path := floors(t, "host 82\n")

	var out bytes.Buffer
	if err := checkFloors(measured, map[string]float64{"host": 82}, path, &out); err != nil {
		t.Fatalf("a package above its floor failed: %v", err)
	}
	if !strings.Contains(out.String(), "90.0%") || !strings.Contains(out.String(), "82.0%") {
		t.Errorf("output %q does not show both numbers", out.String())
	}
}

// TestWriteFloorsRoundsDown: a floor that follows every fractional point would be an
// edit on every pull request, so it is rounded down and churn stays invisible.
func TestWriteFloorsRoundsDown(t *testing.T) {
	path := floors(t, "host 80\n")
	measured := map[string]Package{
		"host": {Name: "host", Statements: 100, Covered: 82},
		"tool": {Name: "tool", Statements: 100, Covered: 91},
	}
	if err := writeFloors(path, measured, map[string]float64{"host": 80}, &bytes.Buffer{}); err != nil {
		t.Fatalf("writeFloors: %v", err)
	}
	body := readFile(t, path)
	for _, want := range []string{"host", "82", "tool", "91"} {
		if !strings.Contains(body, want) {
			t.Errorf("floor file %q does not contain %q", body, want)
		}
	}
}

// TestWriteFloorsKeepsSuiteEntries: "-" is a decision a human made, and a regeneration
// of the file is not allowed to turn it into a number nobody chose.
func TestWriteFloorsKeepsSuiteEntries(t *testing.T) {
	path := floors(t, "sandbox/sandboxtest -\n")
	measured := map[string]Package{
		"sandbox/sandboxtest": {Name: "sandbox/sandboxtest", Statements: 300, Covered: 0},
	}
	if err := writeFloors(path, measured, map[string]float64{"sandbox/sandboxtest": noFloorValue}, &bytes.Buffer{}); err != nil {
		t.Fatalf("writeFloors: %v", err)
	}
	if !strings.Contains(readFile(t, path), "sandbox/sandboxtest") {
		t.Fatalf("the suite entry was dropped:\n%s", readFile(t, path))
	}
}

// TestWriteFloorsRefusesAZeroPackage: 0.0% is either a conformance suite, which a human
// marks "-", or a package whose tests are gone. Writing 0 for it would make the gate
// pass on the failure it exists for.
func TestWriteFloorsRefusesAZeroPackage(t *testing.T) {
	path := floors(t, "")
	measured := map[string]Package{
		"host": {Name: "host", Statements: 100, Covered: 0},
	}
	err := writeFloors(path, measured, map[string]float64{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a package with no coverage was written as a floor")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("error %q does not name the package", err)
	}
}

// TestReadFloorsRejectsGarbage: a malformed floor file is a gate that silently reads
// fewer floors than it looks like it has.
func TestReadFloorsRejectsGarbage(t *testing.T) {
	for name, body := range map[string]string{
		"missing value": "host\n",
		"not a number":  "host eighty\n",
		"out of range":  "host 140\n",
		"empty":         "# only a comment\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readFloors(floors(t, body)); err == nil {
				t.Fatal("a malformed floor file was accepted")
			}
		})
	}
}

// TestRunRequiresItsFlags: the tool is called from the Makefile, so a missing flag has to
// fail rather than gate nothing.
func TestRunRequiresItsFlags(t *testing.T) {
	if err := run(nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("run with no flags succeeded")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
