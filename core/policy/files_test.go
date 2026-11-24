package policy_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
)

func TestFilesNormalizedFoldsUnrestricted(t *testing.T) {
	// A filesystem root and an omitted list are the same permission, so they
	// must have one spelling; otherwise two configs that mean the same thing
	// produce two digests.
	for _, read := range [][]string{nil, {}, {pathtest.Roots()}, {pathtest.Roots(), pathtest.Abs("srv")}} {
		got, err := policy.Files{Read: read}.Normalized()
		if err != nil {
			t.Fatalf("Files{Read: %v}.Normalized(): %v", read, err)
		}
		if got.Read != nil {
			t.Fatalf("Files{Read: %v} normalized to %v, want nil (unrestricted)", read, got.Read)
		}
	}
}

func TestFilesNormalizedPrunesNestedRoots(t *testing.T) {
	got, err := policy.Files{
		Read:  []string{pathtest.Abs("srv/work/sub"), pathtest.Abs("srv/work"), pathtest.Abs("srv/work")},
		Write: []string{pathtest.Abs("srv/work/out"), pathtest.Abs("srv/work/out")},
	}.Normalized()
	if err != nil {
		t.Fatalf("Normalized: %v", err)
	}
	if want := []string{pathtest.Clean("srv/work")}; !reflect.DeepEqual(got.Read, want) {
		t.Fatalf("Read = %v, want %v", got.Read, want)
	}
	if want := []string{pathtest.Clean("srv/work/out")}; !reflect.DeepEqual(got.Write, want) {
		t.Fatalf("Write = %v, want %v", got.Write, want)
	}
}

func TestFilesNormalizedSortsForOneSpelling(t *testing.T) {
	a, err := policy.Files{Read: []string{pathtest.Abs("b"), pathtest.Abs("a")}, Write: []string{pathtest.Abs("a/b"), pathtest.Abs("a/x")}}.Normalized()
	if err != nil {
		t.Fatalf("Normalized: %v", err)
	}
	b, err := policy.Files{Read: []string{pathtest.Abs("a"), pathtest.Abs("b")}, Write: []string{pathtest.Abs("a/x"), pathtest.Abs("a/b")}}.Normalized()
	if err != nil {
		t.Fatalf("Normalized: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("declaration order changed the result:\n%v\n%v", a, b)
	}
}

// TestFilesRejectsUnenforceable is the load-time gate. Every case here is a
// configuration whose meaning an operator would not expect, so it is an error
// rather than a best effort.
func TestFilesRejectsUnenforceable(t *testing.T) {
	for name, files := range map[string]policy.Files{
		"readonly with write":       {ReadOnly: true, Write: []string{pathtest.Abs("srv/out")}},
		"narrowed read, open write": {Read: []string{pathtest.Abs("srv/w")}},
		"write outside read":        {Read: []string{pathtest.Abs("srv/w")}, Write: []string{pathtest.Abs("tmp")}},
		"relative root":             {Read: []string{"srv/w"}, Write: []string{"srv/w"}},
		"glob root":                 {Read: []string{"/srv/*"}},
		"empty root":                {Read: []string{""}},
		"whitespace around a root":  {Read: []string{" /srv"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := files.Normalized(); err == nil {
				t.Fatalf("Normalized() accepted %+v", files)
			}
		})
	}
}

func TestFilesAcceptsTheDocumentedCombinations(t *testing.T) {
	for name, files := range map[string]policy.Files{
		"fully open":           {},
		"read only everywhere": {ReadOnly: true},
		"work directory pair":  {Read: []string{pathtest.Abs("srv/w")}, Write: []string{pathtest.Abs("srv/w")}},
		"write only a subtree": {Read: []string{pathtest.Abs("srv/w")}, Write: []string{pathtest.Abs("srv/w/out")}},
		"read only a subtree":  {Read: []string{pathtest.Abs("srv/w")}, ReadOnly: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := files.Normalized(); err != nil {
				t.Fatalf("Normalized() rejected %+v: %v", files, err)
			}
		})
	}
}

func TestFilesUnrestrictedHandsTheBoundaryItsShape(t *testing.T) {
	read, write, denyWrite := policy.Files{ReadOnly: true}.Unrestricted()
	if read != nil || write != nil || !denyWrite {
		t.Fatalf("got read=%v write=%v denyWrite=%v", read, write, denyWrite)
	}
}

// FuzzFilesNormalized pins the invariants normalization promises: it either
// refuses the input or returns a form that is already normal (idempotent), which
// is what makes the digest stable.
func FuzzFilesNormalized(f *testing.F) {
	f.Add(pathtest.Abs("a"), pathtest.Abs("a/b"), "", "")
	f.Add(pathtest.Roots(), pathtest.Abs("srv"), "", "")
	f.Add("rel", pathtest.Abs("srv"), "*", "")
	f.Add(pathtest.Abs("a/b"), pathtest.Abs("a"), "", "")

	f.Fuzz(func(t *testing.T, read, write, extra, _ string) {
		files := policy.Files{
			Read:  roots(read, extra),
			Write: roots(write),
		}
		got, err := files.Normalized()
		if err != nil {
			return
		}
		again, err := got.Normalized()
		if err != nil {
			t.Fatalf("a normal form failed to normalize: %v (%+v)", err, got)
		}
		if !reflect.DeepEqual(got, again) {
			t.Fatalf("normalization is not idempotent:\n%+v\n%+v", got, again)
		}
	})
}

// roots turns fuzz strings into absolute-ish candidates, keeping the interesting
// shapes (empty, relative, glob, root) reachable.
func roots(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

// TestAsDenialSurvivesWrapping is the contract the recording layer relies on: a
// refusal stays inspectable after a caller wraps it with context.
func TestAsDenialSurvivesWrapping(t *testing.T) {
	_, err := policy.Grant{}.Narrow(policy.Request{
		Models: []model.ModelRef{{ID: model.ModelID{Provider: "openai", Name: "gpt-4o"}}},
	})
	denial, ok := policy.AsDenial(err)
	if !ok {
		t.Fatalf("Narrow returned %v, want a *policy.Denial", err)
	}
	if denial.Check != policy.CheckModels {
		t.Fatalf("Check = %q, want %q", denial.Check, policy.CheckModels)
	}
	if !errdefs.IsPolicyDenied(err) {
		t.Fatal("the denial is not classified as a policy denial")
	}
	if _, ok := policy.AsDenial(errors.Join(errors.New("wrap"), err)); !ok {
		t.Fatal("the denial is not reachable through a wrapped error")
	}
}

// TestRestrictsWrites is the predicate a backend asks before accepting a confine: it says whether
// the policy states anything about writing, which is the difference between "unlimited" and
// "none".
func TestRestrictsWrites(t *testing.T) {
	for name, tc := range map[string]struct {
		files policy.Files
		want  bool
	}{
		"nothing stated":  {policy.Files{}, false},
		"read only":       {policy.Files{ReadOnly: true}, true},
		"write set":       {policy.Files{Read: []string{pathtest.Abs("srv")}, Write: []string{pathtest.Abs("srv")}}, true},
		"read, unlimited": {policy.Files{Read: []string{pathtest.Abs("srv")}}, false},
	} {
		if got := tc.files.RestrictsWrites(); got != tc.want {
			t.Fatalf("%s: RestrictsWrites = %v, want %v", name, got, tc.want)
		}
	}
}
