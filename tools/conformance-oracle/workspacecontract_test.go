package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The workspace dump is committed and compared byte for byte, so the harness has
// to be a function of the cases and of core -- nothing else. These checks fail
// on the ways that goes wrong quietly:
//
//   - an operation that answers "ok" and carries a refusal sentence, or the
//     reverse, which the port would replay as a contradiction rather than as a
//     decision;
//   - a row that no longer declares the view its `why` talks about: a scope with
//     no pattern, a prefixed view with no prefix, a raw row with either;
//   - a violation record that names a rule core does not have, or a path the
//     caller did not write, or one logged for an operation that succeeded;
//   - a pattern row that lost its witness for one of the four forms, so a form
//     can stop working without a row going red;
//   - a walk that visits a file before its child, which would make the order the
//     rows pin something other than "directories first";
//   - two dumps of the same cases that differ, which would mean map iteration
//     leaked into the document.

func generateWorkspace(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := writeWorkspaceContract(&buffer); err != nil {
		t.Fatalf("writeWorkspaceContract: %v", err)
	}
	return buffer.Bytes()
}

func decodeWorkspace(t *testing.T) wsContract {
	t.Helper()
	var document wsContract
	if err := json.Unmarshal(generateWorkspace(t), &document); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return document
}

// workspaceCase finds one row by name.
func workspaceCase(t *testing.T, name string) wsCase {
	t.Helper()
	for _, testCase := range decodeWorkspace(t).Cases {
		if testCase.Name == name {
			return testCase
		}
	}
	t.Fatalf("no case named %q", name)
	return wsCase{}
}

func TestWorkspaceContractIsReproducible(t *testing.T) {
	if first, second := generateWorkspace(t), generateWorkspace(t); !bytes.Equal(first, second) {
		t.Fatal("two dumps of the same cases differ")
	}
}

// TestWorkspaceNothingIsMasked: the medium is a map, so the document claims that
// no string in it depends on the run. If a mask ever appears, the replay stops
// comparing the thing it was masked for -- exactly where a mismatch would hide.
func TestWorkspaceNothingIsMasked(t *testing.T) {
	for _, line := range decodeWorkspace(t).Masked {
		if !strings.HasPrefix(line, "nothing:") {
			t.Errorf("a real mask appeared in a document that claims to have none: %q", line)
		}
	}
}

// TestWorkspaceOpsAgreeWithTheirOk: `ok` and `error` are two spellings of one
// fact. The port derives `ok` from the sentence, so a document that disagreed
// with itself would be replayed as agreement with the wrong answer.
func TestWorkspaceOpsAgreeWithTheirOk(t *testing.T) {
	for _, testCase := range decodeWorkspace(t).Cases {
		for _, op := range testCase.Ops {
			if op.OK != (op.Error == "") {
				t.Errorf("%s: %s %q: ok=%t but error=%q", testCase.Name, op.Op, op.Path, op.OK, op.Error)
			}
		}
	}
}

func TestWorkspaceCasesAreNamedAndExplained(t *testing.T) {
	seen := map[string]bool{}
	for _, testCase := range decodeWorkspace(t).Cases {
		if testCase.Name == "" {
			t.Error("a case has no name, so a mismatch in it cannot be attributed")
		}
		if seen[testCase.Name] {
			t.Errorf("%q appears twice", testCase.Name)
		}
		seen[testCase.Name] = true
		if testCase.Why == "" {
			t.Errorf("%s says nothing about what it is evidence of", testCase.Name)
		}
		if len(testCase.Ops) == 0 {
			t.Errorf("%s asks nothing", testCase.Name)
		}
	}
}

// TestWorkspaceCasesDeclareTheirView: kind, prefixes and the three pattern lists
// are what the port builds its view from. A mismatch between them and the `why`
// produces a row that passes while testing a view nobody meant to build.
func TestWorkspaceCasesDeclareTheirView(t *testing.T) {
	for _, testCase := range decodeWorkspace(t).Cases {
		patterns := len(testCase.DenyRead) + len(testCase.AllowWrite) + len(testCase.MandatoryDeny)
		switch testCase.Kind {
		case "", "raw":
			if patterns > 0 || len(testCase.Prefixes) > 0 {
				t.Errorf("%s: a raw row declares a scope or a prefix", testCase.Name)
			}
		case "scope":
			if len(testCase.Prefixes) > 0 {
				t.Errorf("%s: a scoped row also declares prefixes", testCase.Name)
			}
		case "sub":
			if len(testCase.Prefixes) == 0 {
				t.Errorf("%s: a prefixed row with no prefix is the medium", testCase.Name)
			}
			if patterns > 0 {
				t.Errorf("%s: a prefixed row also declares a scope", testCase.Name)
			}
		default:
			t.Errorf("%s: unknown view kind %q", testCase.Name, testCase.Kind)
		}
	}
	// A scope with no pattern is a view too (`scope-limited` is one: it pins what
	// the view answers about the read cap before any rule is consulted). What must
	// not happen is a document where one of the three lists has no row at all,
	// because that direction could then regress with every replay green.
	var denyRead, allowWrite, mandatoryDeny int
	for _, testCase := range decodeWorkspace(t).Cases {
		if len(testCase.DenyRead) > 0 {
			denyRead++
		}
		if len(testCase.AllowWrite) > 0 {
			allowWrite++
		}
		if len(testCase.MandatoryDeny) > 0 {
			mandatoryDeny++
		}
	}
	for name, count := range map[string]int{
		"deny_read":      denyRead,
		"allow_write":    allowWrite,
		"mandatory_deny": mandatoryDeny,
	} {
		if count == 0 {
			t.Errorf("no row declares %s, so that direction is not pinned", name)
		}
	}
}

// workspaceLocalCase finds one real-tree row by name.
func workspaceLocalCase(t *testing.T, name string) wsLocalCase {
	t.Helper()
	for _, testCase := range decodeWorkspace(t).Local.Cases {
		if testCase.Name == name {
			return testCase
		}
	}
	t.Fatalf("no local case named %q", name)
	return wsLocalCase{}
}

// TestWorkspaceLocalCasesAreShaped: the real-tree rows carry a decision the map
// rows do not -- which errors the *host* worded -- so the rules that keep that
// decision honest are worth stating.
//
// `TestWorkspaceContractIsReproducible` is what proves the masking claim: every
// dump builds a fresh temporary tree, and the section still has to be byte
// identical, so no row may name the directory it ran in.
func TestWorkspaceLocalCasesAreShaped(t *testing.T) {
	document := decodeWorkspace(t)
	if len(document.Local.Cases) == 0 {
		t.Fatal("the local section has no cases")
	}
	seen := map[string]bool{}
	var internal int
	for _, testCase := range document.Local.Cases {
		if testCase.Name == "" {
			t.Error("a local case has no name, so a mismatch in it cannot be attributed")
		}
		if seen[testCase.Name] {
			t.Errorf("local case %q appears twice", testCase.Name)
		}
		seen[testCase.Name] = true
		if testCase.Why == "" {
			t.Errorf("%s says nothing about what it is evidence of", testCase.Name)
		}
		if len(testCase.Ops) == 0 && testCase.Build == "" {
			t.Errorf("%s asks nothing and builds nothing", testCase.Name)
		}
		if testCase.Build != "" && (testCase.BuildKind == "" || testCase.BuildKind == "internal") {
			t.Errorf("%s: a view refused with %q, which is not a refusal core worded", testCase.Name, testCase.BuildKind)
		}
		if len(testCase.Final) == 0 {
			t.Errorf("%s records no tree, so the fixtures' fate is not pinned", testCase.Name)
		}
		for _, op := range testCase.Ops {
			if op.OK != (op.Error == "") {
				t.Errorf("%s: %s %q: ok=%t but error=%q", testCase.Name, op.Op, op.Path, op.OK, op.Error)
			}
			if op.OK != (op.Kind == "") {
				t.Errorf("%s: %s %q: ok=%t but kind=%q", testCase.Name, op.Op, op.Path, op.OK, op.Kind)
			}
			if op.Kind == "internal" {
				internal++
			}
		}
	}
	if internal == 0 {
		t.Error("no row failed with a sentence the host wrote, so the mask hides nothing and claims nothing")
	}
	for _, line := range document.Local.Masked {
		if !strings.Contains(line, "internal") {
			t.Errorf("the local section masks %q, which is not the host's sentence", line)
		}
	}
}

// TestWorkspaceLocalCasesPinWhatTheTreeDecides: each of these is a rule that only
// a real tree can show, and each is one an implementation could lose while every
// map-medium row stayed green -- so each needs a row that names it.
func TestWorkspaceLocalCasesPinWhatTheTreeDecides(t *testing.T) {
	document := decodeWorkspace(t)
	type witness struct {
		path string
		op   string
		ok   bool
	}
	witnesses := map[string]witness{
		// A link inside the tree is followed ...
		"relative link followed": {path: "link-in", op: "read", ok: true},
		// ... an absolute one is refused even when it points back inside ...
		"absolute link refused": {path: "link-abs-in", op: "read", ok: false},
		// ... and one that leaves is refused too.
		"escaping link refused": {path: "link-out", op: "read", ok: false},
		// The view a link made escape is refused before any operation runs.
		"escaping prefix refused": {path: "", op: "", ok: false},
		// Writes go through an in-tree link to the directory it names.
		"write through a link": {path: "link-dir/new.txt", op: "write", ok: true},
		// The real backend is the one that reports its capabilities.
		"capabilities reported": {path: "", op: "capabilities", ok: true},
	}
	found := map[string]bool{}
	for _, testCase := range document.Local.Cases {
		for _, op := range testCase.Ops {
			for name, want := range witnesses {
				if want.op != "" && op.Op == want.op && op.Path == want.path && op.OK == want.ok {
					found[name] = true
				}
			}
		}
		if testCase.Build != "" && strings.Contains(testCase.Build, "symlink escape") {
			found["escaping prefix refused"] = true
		}
		// A row with an outside area proves a removal never touched the target.
		if len(testCase.FinalOutside) > 0 {
			found["the target survived"] = true
		}
	}
	for name := range witnesses {
		if !found[name] {
			t.Errorf("no row pins %s", name)
		}
	}
	if !found["the target survived"] {
		t.Error("no row shows what `remove_all` on a link left behind, so \"the link, never the target\" is unpinned")
	}

	// An escaping link is what makes `delete` and `remove_all` differ: `delete`
	// stats it and fails, `remove_all` unlinks it. Both rows come from one case.
	symlinks := workspaceLocalCase(t, "local-symlinks")
	var deleted, removed bool
	for _, op := range symlinks.Ops {
		if op.Path != "link-out" {
			continue
		}
		if op.Op == "delete" && !op.OK {
			deleted = true
		}
		if op.Op == "remove_all" && op.OK {
			removed = true
		}
	}
	if !deleted || !removed {
		t.Error("local-symlinks no longer shows `delete` failing and `remove_all` succeeding on one escaping link")
	}
}

// workspaceViolationReasons are the three rules ScopedWorkspace refuses a
// direction with. The record names the rule, not the pattern that matched it.
var workspaceViolationReasons = map[string]bool{
	"mandatory deny":            true,
	"deny-read pattern matched": true,
	"not in allow-write list":   true,
}

func TestWorkspaceViolationsNameTheRuleAndThePath(t *testing.T) {
	for _, testCase := range decodeWorkspace(t).Cases {
		for _, op := range testCase.Ops {
			for _, violation := range op.Violations {
				if !workspaceViolationReasons[violation.Reason] {
					t.Errorf("%s: %s logged a rule core does not have: %q", testCase.Name, op.Op, violation.Reason)
				}
				// A rename checks both ends, so the record may name either path the
				// caller wrote -- but only one of them.
				if violation.Path != op.Path && violation.Path != op.Arg {
					t.Errorf("%s: the record names %q, and the caller wrote %q and %q", testCase.Name, violation.Path, op.Path, op.Arg)
				}
				if violation.Operation != "read" && violation.Operation != "write" {
					t.Errorf("%s: a violation names the direction %q", testCase.Name, violation.Operation)
				}
				if op.OK {
					t.Errorf("%s: %s %q is logged as a violation and answered ok", testCase.Name, op.Op, op.Path)
				}
			}
		}
	}
}

// TestWorkspacePatternFormsEachHaveAWitness: the row's claim is that each of the
// four forms is a decision with a name, so deleting a form must flip a path it
// covers. The witnesses are written out rather than derived from the answers,
// because what the document records is *that* the scope refused, not which
// pattern did it -- and a witness that is refused for some other reason would
// leave the claim standing while the form was gone.
func TestWorkspacePatternFormsEachHaveAWitness(t *testing.T) {
	witnesses := map[string]string{
		"dir/**":      "dir", // the directory itself, which `dir/**` covers
		"**/cache/**": "a/cache/b",
		"**/*.key":    "a/b/c.key",
		"*.tmp":       "x.tmp",
	}
	nearMisses := []string{
		"xdir/x",        // a name that ends like the directory but is not it
		"a/b/c.key.bak", // a suffix that is not the pattern's
		"d/x.tmp",       // a separator-free pattern only reaches the root
	}

	testCase := workspaceCase(t, "scope-pattern-forms")
	if len(testCase.DenyRead) != len(witnesses) {
		t.Fatalf("the row lists %d patterns; the witnesses name %d", len(testCase.DenyRead), len(witnesses))
	}
	refused := map[string]bool{}
	for _, op := range testCase.Ops {
		if len(op.Violations) > 0 {
			refused[op.Path] = true
		}
	}
	for pattern, path := range witnesses {
		if !slices.Contains(testCase.DenyRead, pattern) {
			t.Errorf("the row no longer lists %q", pattern)
		}
		if !refused[path] {
			t.Errorf("%q has no witness: %q is not refused by the scope", pattern, path)
		}
	}
	for _, path := range nearMisses {
		if refused[path] {
			t.Errorf("%q is refused, which says a form reaches further than it should", path)
		}
	}
}

// TestWorkspaceCappedRowsSayDifferentThings: the two rows exist to tell the
// bounded read apart from the measured one. A port that asked the type question
// and one that called a method and hoped would produce the same sentence
// otherwise, and no row would notice.
func TestWorkspaceCappedRowsSayDifferentThings(t *testing.T) {
	oversized := func(name string) string {
		for _, op := range workspaceCase(t, name).Ops {
			if op.Op == "capped" && op.Max == 9 {
				return op.Error
			}
		}
		t.Fatalf("%s has no 9-byte read", name)
		return ""
	}
	bounded, measured := oversized("capped-bounded"), oversized("capped-fallback")
	if bounded == measured {
		t.Fatalf("both media refuse the same way: %q", bounded)
	}
	for _, sentence := range []string{bounded, measured} {
		if !strings.Contains(sentence, "a.txt") || !strings.Contains(sentence, "9") {
			t.Errorf("a refusal stopped naming the file and the cap: %q", sentence)
		}
	}
}

// TestWorkspaceWalkVisitsADirectoryBeforeItsContents pins the order the row
// claims, which a replay can only compare: a document that visited a file before
// its children would still be reproducible and still be replayed.
func TestWorkspaceWalkVisitsADirectoryBeforeItsContents(t *testing.T) {
	testCase := workspaceCase(t, "walk-order")
	if len(testCase.Ops) < 2 || testCase.Ops[0].Op != "walk" || testCase.Ops[1].Op != "walk_skip" {
		t.Fatalf("the row no longer opens with a walk and a skipping walk")
	}
	full, skipped := testCase.Ops[0].List, testCase.Ops[1].List

	for i, entry := range full {
		for _, later := range full[i+1:] {
			if strings.HasPrefix(later.Path, entry.Path+"/") && !entry.Dir {
				t.Errorf("%s is a file and its child %s follows it", entry.Path, later.Path)
			}
		}
	}

	present := func(entries []wsEntry, path string) bool {
		for _, entry := range entries {
			if entry.Path == path {
				return true
			}
		}
		return false
	}
	for _, gone := range []string{"dir/sub", "dir/sub/c.go", "dir/sub/deep", "dir/sub/deep/d.go"} {
		if present(skipped, gone) {
			t.Errorf("the skipped subtree still lists %q", gone)
		}
	}
	if !present(skipped, "dir") {
		t.Error("skipping a subdirectory also removed its parent")
	}
}
