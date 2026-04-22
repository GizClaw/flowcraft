package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidFixtures asserts every contracts under testdata/valid loads + lints clean.
func TestValidFixtures(t *testing.T) {
	root := filepath.Join("testdata", "valid")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no valid fixtures present")
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			manifest := filepath.Join(root, name, "contracts", "events.yaml")
			sp, err := loadSpec(manifest)
			if err != nil {
				t.Fatalf("loadSpec: %v", err)
			}
			if errs := lintSpec(sp); len(errs) > 0 {
				for _, e := range errs {
					t.Error(e)
				}
			}
		})
	}
}

// TestInvalidFixtures asserts every contracts under testdata/invalid surfaces a
// lint or load error containing a substring derived from the directory name.
//
// The mapping is intentionally fuzzy: the test only fails when no reported error
// mentions ANY of the expected substrings. This keeps fixtures robust against
// minor wording changes while still confirming the right rule fired.
func TestInvalidFixtures(t *testing.T) {
	root := filepath.Join("testdata", "invalid")
	type expect struct{ name string; needles []string }
	cases := []expect{
		{"bad_name", []string{"name must match"}},
		{"imperative_verb", []string{"imperative", "verb whitelist"}},
		{"ing_suffix", []string{"-ing"}},
		{"unknown_partition", []string{"partition", "not registered"}},
		{"unknown_category", []string{"category", "not in manifest"}},
		{"audit_no_summary", []string{"audit_summary empty"}},
		{"audit_summary_no_payload_ref", []string{"{{payload."}},
		{"unknown_field_type", []string{"unknown type"}},
		{"array_no_item_type", []string{"item_type"}},
		{"missing_doc", []string{"doc required"}},
		{"missing_producers", []string{"producers required"}},
		{"off_whitelist_verb", []string{"verb whitelist"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			manifest := filepath.Join(root, c.name, "contracts", "events.yaml")
			sp, loadErr := loadSpec(manifest)
			var errs []error
			if loadErr != nil {
				errs = append(errs, loadErr)
			} else {
				errs = lintSpec(sp)
			}
			if len(errs) == 0 {
				t.Fatalf("expected lint failures for %q, got none", c.name)
			}
			joined := ""
			for _, e := range errs {
				joined += e.Error() + "\n"
			}
			matched := false
			for _, n := range c.needles {
				if strings.Contains(joined, n) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("expected one of %v in errors, got:\n%s", c.needles, joined)
			}
		})
	}
}

// TestCompatFixtures runs the §3.7 compatibility matrix against fixture pairs
// (baseline.snapshot.yaml + contracts/) under testdata/compat.
//
// Each case declares whether checkEvolution must report at least one error and,
// if so, a substring that the consolidated error output must contain.
func TestCompatFixtures(t *testing.T) {
	type expect struct {
		name        string
		wantErr     bool
		wantNeedle  string
	}
	cases := []expect{
		{"add_optional_field", false, ""},
		{"add_required_field", true, "new payload field"},
		{"remove_field", true, "payload field"},
		{"change_type", true, "type changed"},
		{"tighten_required", true, "tightened optional -> required"},
		{"loosen_required", false, ""},
		{"version_bump_breaking_ok", false, ""},
		{"version_regress", true, "version regressed"},
		{"change_category_same_version", true, "category changed"},
		{"remove_event_undeprecated", true, "removed without prior deprecated"},
		{"remove_event_deprecated", false, ""},
		{"rename_field", true, "payload field"},
	}
	root := filepath.Join("testdata", "compat")
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			manifest := filepath.Join(root, c.name, "contracts", "events.yaml")
			baseline := filepath.Join(root, c.name, "baseline.snapshot.yaml")
			sp, err := loadSpec(manifest)
			if err != nil {
				t.Fatalf("loadSpec: %v", err)
			}
			if errs := lintSpec(sp); len(errs) > 0 {
				for _, e := range errs {
					t.Errorf("unexpected lint error: %v", e)
				}
			}
			evo := checkEvolution(sp, baseline)
			if c.wantErr {
				if len(evo) == 0 {
					t.Fatalf("expected evolution failure for %q, got none", c.name)
				}
				joined := ""
				for _, e := range evo {
					joined += e.Error() + "\n"
				}
				if !strings.Contains(joined, c.wantNeedle) {
					t.Errorf("expected substring %q in errors, got:\n%s", c.wantNeedle, joined)
				}
			} else {
				if len(evo) > 0 {
					for _, e := range evo {
						t.Errorf("unexpected evolution error: %v", e)
					}
				}
			}
		})
	}
}

// TestGoldenCase1 regenerates artefacts for testdata/golden/case1 into a tmp dir
// and diffs them against want/. Catches accidental codegen template drift.
func TestGoldenCase1(t *testing.T) {
	src := filepath.Join("testdata", "golden", "case1")
	manifest := filepath.Join(src, "contracts", "events.yaml")
	sp, err := loadSpec(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if errs := lintSpec(sp); len(errs) > 0 {
		for _, e := range errs {
			t.Error(e)
		}
		return
	}

	tmp := t.TempDir()
	if err := writeGoOutputs(sp, filepath.Join(tmp, "internal", "eventlog")); err != nil {
		t.Fatal(err)
	}
	if err := writeTSOutputs(sp, filepath.Join(tmp, "web", "src", "api")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONSchemas(sp, filepath.Join(tmp, "contracts")); err != nil {
		t.Fatal(err)
	}

	wantRoot := filepath.Join(src, "want")
	files := []string{
		"internal/eventlog/events_gen.go",
		"internal/eventlog/payloads_gen.go",
		"internal/eventlog/partitions_gen.go",
		"internal/eventlog/dispatch_gen.go",
		"internal/eventlog/publish_gen.go",
		"internal/eventlog/audit_gen.go",
		"web/src/api/events.gen.ts",
		"web/src/api/event_payloads.gen.ts",
		"web/src/api/event_guards.gen.ts",
		"contracts/envelope.schema.json",
		"contracts/payloads.schema.json",
	}
	for _, rel := range files {
		got, err := os.ReadFile(filepath.Join(tmp, rel))
		if err != nil {
			t.Errorf("read got %s: %v", rel, err)
			continue
		}
		want, err := os.ReadFile(filepath.Join(wantRoot, rel))
		if err != nil {
			t.Errorf("read want %s: %v", rel, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("golden drift in %s: regenerate via go run ./cmd/eventgen -repo=%s -mode=gen and update want/", rel, src)
		}
	}
}
