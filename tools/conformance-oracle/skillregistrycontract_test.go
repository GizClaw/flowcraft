package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The registry dump is committed and compared byte for byte, so the harness has
// to be a function of the trees and of core -- nothing else. These checks fail
// on the ways that goes wrong quietly:
//
//   - a mask that stopped matching the strings it claims to cover, which looks
//     exactly like a mask with nothing to do;
//   - a tree that no longer demonstrates what its `why` says: an escaping
//     symlink that silently became a real directory, a hidden entry that moved
//     out of the tree, a repeated root that stopped repeating;
//   - a row whose paths are not logical, which makes a mismatch unattributable
//     and a replay impossible;
//   - two dumps of the same trees that differ, which would mean map iteration or
//     a temporary directory leaked into the document.

func generateRegistry(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := writeSkillRegistryContract(&buffer); err != nil {
		t.Fatalf("writeSkillRegistryContract: %v", err)
	}
	return buffer.Bytes()
}

func decodeRegistry(t *testing.T) registryContract {
	t.Helper()
	var document registryContract
	if err := json.Unmarshal(generateRegistry(t), &document); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return document
}

func TestRegistryContractIsReproducible(t *testing.T) {
	if first, second := generateRegistry(t), generateRegistry(t); !bytes.Equal(first, second) {
		t.Fatal("two dumps of the same trees differ")
	}
}

// TestRegistryMaskKeepsWhatCoreWrote: the masked half of a string is somebody
// else's words, and the kept half is core's. A mask that swallowed the whole
// sentence would pass every replay while pinning nothing.
func TestRegistryMaskKeepsWhatCoreWrote(t *testing.T) {
	const (
		given     = "/var/folders/xy/skill-registry-contract-123"
		canonical = "/private" + given
	)
	mask := func(text string) string { return maskRegistryText(text, given, canonical) }
	cases := []struct{ name, text, want string }{
		{
			name: "a canonical path is masked even though the harness gave another form",
			text: canonical + "/a/alpha/SKILL.md",
			want: "a/alpha/SKILL.md",
		},
		{
			name: "a path under the given form is masked too",
			text: given + "/b/dup/SKILL.md",
			want: "b/dup/SKILL.md",
		},
		{
			name: "a stat failure keeps the verb and the path",
			text: "stat " + canonical + "/a/dangling: lstat: no such file or directory",
			want: "stat a/dangling: <os error>",
		},
		{
			name: "a directory that cannot be opened keeps its verb and path",
			text: "open " + given + "/a: permission denied",
			want: "open a: <os error>",
		},
		{
			name: "a dangling symlink masks the operating system's whole sentence",
			text: "resolve symlink: lstat " + canonical + "/a/dangling: no such file or directory",
			want: "resolve symlink: <os error>",
		},
		{
			name: "core's escape sentence is kept whole",
			text: "symlink escapes the configured skill roots",
			want: "symlink escapes the configured skill roots",
		},
		{
			name: "a library's decode error is masked after the classification",
			text: "skill: parse frontmatter: error converting YAML to JSON: yaml: line 2: did not find expected key",
			want: "skill: parse frontmatter: <decode error>",
		},
		{
			name: "a refusal with no third-party wording is untouched",
			text: "skill: description is required",
			want: "skill: description is required",
		},
	}
	for _, c := range cases {
		if got := mask(c.text); got != c.want {
			t.Errorf("%s: masked to %q, want %q", c.name, got, c.want)
		}
	}
}

// TestRegistryTreesAreNamedAndUsable: a tree has to be attributable in a
// mismatch, and its entries have to describe one tree of their own -- relative
// paths, parents the harness can create, and links that name a target.
func TestRegistryTreesAreNamedAndUsable(t *testing.T) {
	seen := make(map[string]bool)
	for _, tree := range registryTrees() {
		if tree.Name == "" {
			t.Fatalf("a tree is unnamed: %+v", tree)
		}
		if seen[tree.Name] {
			t.Fatalf("tree %s appears twice", tree.Name)
		}
		seen[tree.Name] = true
		if tree.Why == "" {
			t.Fatalf("tree %s does not say what it is evidence of", tree.Name)
		}
		for _, entry := range tree.Entries {
			if entry.Path == "" || strings.HasPrefix(entry.Path, "/") {
				t.Fatalf("tree %s has a non-relative entry: %+v", tree.Name, entry)
			}
			switch entry.Kind {
			case "link":
				if entry.Target == "" || strings.HasPrefix(entry.Target, "/") {
					t.Fatalf("tree %s has a link without a relative target: %+v", tree.Name, entry)
				}
			case "dir":
				if entry.Data != "" {
					t.Fatalf("tree %s has a directory with bytes: %+v", tree.Name, entry)
				}
			case "":
				if entry.Data == "" {
					t.Fatalf("tree %s has a file without bytes: %+v", tree.Name, entry)
				}
			default:
				t.Fatalf("tree %s has an unknown kind %q", tree.Name, entry.Kind)
			}
		}
		for _, root := range tree.Roots {
			if root == "" || strings.HasPrefix(root, "/") {
				t.Fatalf("tree %s has a non-relative root: %q", tree.Name, root)
			}
		}
		for _, disabled := range tree.Disabled {
			if disabled.Kind != "name" && disabled.Kind != "path" {
				t.Fatalf("tree %s disables %q with an unknown kind %q", tree.Name, disabled.Value, disabled.Kind)
			}
			if disabled.Kind == "path" && strings.HasPrefix(disabled.Value, "/") {
				t.Fatalf("tree %s disables an absolute path: %q", tree.Name, disabled.Value)
			}
		}
	}
}

// TestRegistryAnswersAreMasked: every path in an answer is the logical one, so
// a mismatch names the entry a reader can find in the tree rather than a
// temporary directory that exists only for the length of one run.
func TestRegistryAnswersAreMasked(t *testing.T) {
	document := decodeRegistry(t)
	if len(document.Trees) < 10 {
		t.Fatalf("the table has %d trees", len(document.Trees))
	}
	for _, tree := range document.Trees {
		answers := []string{}
		for _, skill := range tree.Discover.Skills {
			answers = append(answers, skill.Path)
		}
		answers = append(answers, tree.Discover.Roots...)
		answers = append(answers, tree.Discover.ScanRoots...)
		for _, diagnostic := range tree.Discover.Diagnostics {
			answers = append(answers, diagnostic.Path)
		}
		if service := tree.Service; service != nil {
			for _, skill := range service.List {
				answers = append(answers, skill.Path)
			}
			answers = append(answers, service.Roots...)
			for _, mention := range service.Mentions {
				for _, skill := range mention.Answer {
					answers = append(answers, skill.Path)
				}
			}
			for _, ranking := range service.Rankings {
				for _, scored := range ranking.Answer {
					answers = append(answers, scored.Path)
				}
			}
			for _, read := range service.Reads {
				answers = append(answers, read.Answer.Path)
			}
		}
		for _, answer := range answers {
			if strings.HasPrefix(answer, "/") || strings.Contains(answer, "skill-registry-contract-") {
				t.Fatalf("tree %s carries an unmasked path: %q", tree.Name, answer)
			}
		}
	}
}

// TestRegistryTreesDemonstrateTheirClaims: a fixture that quietly stopped
// containing its corner case would still replay, and would pin nothing. These
// are the claims the trees exist for, asserted against core's own answers.
func TestRegistryTreesDemonstrateTheirClaims(t *testing.T) {
	document := decodeRegistry(t)
	find := func(name string) registryTree {
		t.Helper()
		for _, tree := range document.Trees {
			if tree.Name == name {
				return tree
			}
		}
		t.Fatalf("tree %s is missing from the table", name)
		return registryTree{}
	}

	escape := find("symlink-escape")
	if len(escape.Discover.Skills) != 0 {
		t.Fatalf("the escaping tree discovered %+v", escape.Discover.Skills)
	}
	if len(escape.Discover.Diagnostics) != 2 {
		t.Fatalf("the escaping tree recorded %+v, want one diagnostic per link", escape.Discover.Diagnostics)
	}

	hidden := find("hidden-and-shapes")
	for _, skill := range hidden.Discover.Skills {
		if skill.Name == "hidden" {
			t.Fatal("the hidden entry was discovered")
		}
	}

	repeated := find("repeated-roots")
	if len(repeated.Discover.Roots) != 1 {
		t.Fatalf("the repeated-root tree listed %v, want one root", repeated.Discover.Roots)
	}
	if len(repeated.Discover.ScanRoots) != 3 {
		t.Fatalf("the repeated-root tree scanned %v, want every configured root", repeated.Discover.ScanRoots)
	}

	disabled := find("disabled")
	if len(disabled.Discover.Skills) != 4 {
		t.Fatalf("discovery saw %d skills, want all four: disabled is a service filter", len(disabled.Discover.Skills))
	}
	if len(disabled.Service.List) != 2 {
		t.Fatalf("the disabled tree listed %d skills, want two", len(disabled.Service.List))
	}

	reload := find("reload-sees-new-skills")
	if len(reload.Service.List) != 1 || len(reload.Service.Reload.List) != 2 {
		t.Fatalf("reload answered %v then %v, want one then two",
			reload.Service.List, reload.Service.Reload.List)
	}
}
