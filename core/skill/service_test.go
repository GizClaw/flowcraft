package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestEmptyServiceServesNothing(t *testing.T) {
	svc := NewService(context.Background(), Options{})
	if len(svc.List()) != 0 || len(svc.Diagnostics()) != 0 || len(svc.Roots()) != 0 {
		t.Fatalf("empty service = %+v", svc.List())
	}
	if got := svc.Rank("anything", 5, 0); got != nil {
		t.Fatalf("Rank on empty service = %+v, want nil", got)
	}
	if got := svc.Mentioned("$nothing"); got != nil {
		t.Fatalf("Mentioned on empty service = %+v, want nil", got)
	}
	if _, _, err := svc.ReadFull("missing"); !errdefs.IsNotFound(err) {
		t.Fatalf("ReadFull(missing) err = %v, want not-found", err)
	}
	if got := svc.TopN(); got != defaultTopN {
		t.Fatalf("TopN() = %d, want %d", got, defaultTopN)
	}
}

func TestDisabledFilter(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "keep", "name: keep\ndescription: keep me\n")
	dropPath := canonical(t, writeSkill(t, root, "drop",
		"name: drop\ndescription: retire the backlog\n"))

	svc := NewService(context.Background(), Options{
		Roots:    []string{root},
		Disabled: []string{"drop"},
	})
	for _, sk := range svc.List() {
		if sk.Name == "drop" {
			t.Fatalf("disabled skill drop must be filtered: %+v", svc.List())
		}
	}
	if _, ok := svc.ByName("keep"); !ok {
		t.Fatalf("keep must survive the disabled filter: %+v", svc.List())
	}
	// The index is built after the filter, so a disabled skill cannot
	// rank back in through a search.
	if got := svc.Rank("retire backlog", 5, 0); len(got) != 0 {
		t.Fatalf("Rank(retire backlog) = %+v, want no hits", got)
	}

	// An absolute path disables the same skill without naming it.
	byPath := NewService(context.Background(), Options{
		Roots:    []string{root},
		Disabled: []string{dropPath},
	})
	if _, ok := byPath.ByName("drop"); ok {
		t.Fatal("path-disabled skill must not resolve")
	}
	if _, ok := byPath.ByName("keep"); !ok {
		t.Fatal("keep must survive a path-only disable list")
	}
}

func TestReloadPicksUpNewSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "first", "name: first\ndescription: first skill\n")
	svc := NewService(context.Background(), Options{Roots: []string{root}})
	if _, ok := svc.ByName("first"); !ok {
		t.Fatal("first skill missing after construction")
	}
	writeSkill(t, root, "second", "name: second\ndescription: second skill\n")
	if _, ok := svc.ByName("second"); ok {
		t.Fatal("second skill must not appear before Reload")
	}
	svc.Reload()
	if _, ok := svc.ByName("second"); !ok {
		t.Fatalf("second skill missing after Reload: %+v", svc.List())
	}
	if hits := svc.Rank("second skill", 5, 0); len(hits) == 0 || hits[0].Name != "second" {
		t.Fatalf("Rank(second) = %+v, want second", hits)
	}
}

func TestReadFullAndReadByPath(t *testing.T) {
	root := t.TempDir()
	path := canonical(t, writeSkill(t, root, "review",
		"name: review\ndescription: review code\n"))
	svc := NewService(context.Background(), Options{Roots: []string{root}})

	sk, body, err := svc.ReadFull("review")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Path != path {
		t.Fatalf("ReadFull path = %q, want %q", sk.Path, path)
	}
	if strings.Contains(body, "---") || strings.Contains(body, "name: review") {
		t.Fatalf("body = %q, want frontmatter stripped", body)
	}
	if !strings.Contains(body, "Do the thing.") {
		t.Fatalf("body = %q, want instructions", body)
	}
	if _, _, err := svc.ReadFull("missing"); !errdefs.IsNotFound(err) {
		t.Fatalf("ReadFull(missing) err = %v, want not-found", err)
	}

	byPath, pathBody, err := svc.ReadByPath(path)
	if err != nil {
		t.Fatalf("ReadByPath(%q): %v", path, err)
	}
	if byPath.Name != "review" || pathBody != body {
		t.Fatalf("ReadByPath = (%q, %q), want review with the same body",
			byPath.Name, pathBody)
	}
	// A path outside the snapshot is refused even when the file
	// exists.
	stale := writeSkill(t, t.TempDir(), "stale", "name: stale\ndescription: stale\n")
	if _, _, err := svc.ReadByPath(canonical(t, stale)); err == nil {
		t.Fatal("ReadByPath(stale/outside path) should fail")
	}
}
