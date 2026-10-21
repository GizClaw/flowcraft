package plugin

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/craft/plugin/server"
)

// declared is a manifest that declares every contribution channel, so a
// projection over it has something to refuse.
func declared() Manifest {
	return Manifest{
		ID:      "hello",
		Version: "0.1.0",
		Run:     &RunSpec{Command: "true"},
		Skills:  []string{"skills"},
		Hooks:   []string{"hooks/on_start.json"},
		Nodes:   []NodeDecl{{Type: "echo", Tool: "node_echo"}},
	}
}

// TestStoreApprovesGrantsPerPlugin pins the seam an application decides
// through: the manifest asks, [Options.Approve] answers, and the entry
// holds the answer. Three properties make that answer the authoritative
// one — a refusal narrows what the projection publishes, an approval
// cannot add a grant the manifest never asked for, and a store with no
// policy approves every request, because installing a plugin is already a
// decision.
func TestStoreApprovesGrantsPerPlugin(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", `{
		"id": "hello", "version": "0.1.0",
		"requires": ["tools.provide", "skills.provide", "events.emit"],
		"run": {"command": "true"},
		"skills": ["skills"]
	}`)
	writePlugin(t, root, "quiet", `{
		"id": "quiet", "version": "0.1.0",
		"requires": ["skills.provide"],
		"skills": ["skills"]
	}`)
	for _, id := range []string{"hello", "quiet"} {
		if err := os.MkdirAll(
			filepath.Join(root, id, "skills"), 0o700); err != nil {
			t.Fatalf("mkdir skills: %v", err)
		}
	}
	asked := map[string][]string{}
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
		Grants:      []string{"events.emit"},
		Approve: func(id string, requires []string) []string {
			asked[id] = append([]string(nil), requires...)
			if id != "hello" {
				return requires
			}
			// Refuse skills, and ask for one grant the manifest never
			// required: the extra must not be handed out.
			return []string{"tools.provide", "events.emit", "storage.kv"}
		},
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	entries, err := store.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	byID := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	if want := []string{
		"tools.provide", "skills.provide", "events.emit",
	}; !slices.Equal(asked["hello"], want) {
		t.Fatalf("Approve saw %v, want the manifest's requires %v",
			asked["hello"], want)
	}
	hello := byID["hello"]
	if want := []string{"events.emit", "tools.provide"}; !slices.Equal(
		hello.Grants.List(), want) {
		t.Fatalf("hello grantees = %v, want %v", hello.Grants.List(), want)
	}
	if projection := hello.Projection(); !projection.Tools ||
		len(projection.Skills) != 0 ||
		!slices.Equal(projection.Dropped, []string{"skills"}) {
		t.Fatalf("hello projection = %+v, want tools and a dropped skills",
			projection)
	}
	if quiet := byID["quiet"]; !slices.Equal(
		quiet.Projection().Skills, []string{"skills"}) {
		t.Fatalf("quiet projection = %+v, want its approved skills",
			quiet.Projection())
	}
	summaries, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, summary := range summaries {
		if summary.ID != "hello" {
			continue
		}
		if len(summary.Requires) != 3 || !slices.Equal(
			summary.Granted, []string{"events.emit", "tools.provide"}) {
			t.Fatalf("hello summary = %+v, want both sides of the decision",
				summary)
		}
	}
	open, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
		Grants:      []string{"events.emit"},
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	entries, err = open.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	for _, entry := range entries {
		if entry.ID != "hello" {
			continue
		}
		if len(entry.Grants.List()) != 3 {
			t.Fatalf("unstinted store grants %v, want every request",
				entry.Grants.List())
		}
	}
}

// grantSet is the set a store's approval leaves behind.
func grantSet(grants ...string) server.GrantSet {
	out := make(server.GrantSet, len(grants))
	for _, grant := range grants {
		out[grant] = true
	}
	return out
}

// TestProjectionPublishesOnlyGrantedChannels is the drop table as data:
// for each channel, a manifest that declares everything contributes the
// section of the one grant it holds, and names the channels it was refused
// in channel order instead of leaving them silently missing. The tools
// channel has no manifest section — it is published over the wire, and
// Tools reports the grant — so holding it refuses nothing.
func TestProjectionPublishesOnlyGrantedChannels(t *testing.T) {
	t.Parallel()
	sections := []string{"skills", "hooks", "nodes"}
	allBut := func(skip string) []string {
		out := make([]string, 0, len(sections))
		for _, kind := range sections {
			if kind != skip {
				out = append(out, kind)
			}
		}
		return out
	}
	cases := []struct {
		held    Grant
		tools   bool
		carries string
		dropped []string
	}{
		{GrantTools, true, "", sections},
		{GrantSkills, false, "skills", allBut("skills")},
		{GrantHooks, false, "hooks", allBut("hooks")},
		{GrantNodes, false, "nodes", allBut("nodes")},
		{GrantUI, false, "", sections},
		{GrantStorage, false, "", sections},
	}
	for _, testCase := range cases {
		t.Run(string(testCase.held), func(t *testing.T) {
			t.Parallel()
			projection := declared().project(grantSet(string(testCase.held)))
			if projection.Tools != testCase.tools {
				t.Fatalf("Tools = %v, want %v", projection.Tools, testCase.tools)
			}
			for kind, carried := range map[string]bool{
				"skills": len(projection.Skills) > 0,
				"hooks":  len(projection.Hooks) > 0,
				"nodes":  len(projection.Nodes) > 0,
			} {
				if carried != (kind == testCase.carries) {
					t.Fatalf("%s carried = %v, want %v",
						kind, carried, kind == testCase.carries)
				}
			}
			if !slices.Equal(projection.Dropped, testCase.dropped) {
				t.Fatalf("Dropped = %v, want %v",
					projection.Dropped, testCase.dropped)
			}
		})
	}
}

// TestProjectionPublishesNothingByDefault pins the zero value: a plugin
// nobody granted anything to contributes nothing, which is the answer a
// caller gets when it forgets to think about grants — and therefore the
// one that has to be safe. An invalid or colliding entry takes the same
// route: its manifest parsed, but nothing about it may be published.
func TestProjectionPublishesNothingByDefault(t *testing.T) {
	t.Parallel()
	empty := declared().project(nil)
	if empty.Tools || len(empty.Skills) > 0 || len(empty.Hooks) > 0 ||
		len(empty.Nodes) > 0 {
		t.Fatalf("projection without grants = %+v, want nothing", empty)
	}
	entry := Entry{
		ID:       "hello",
		Manifest: declared(),
		Grants:   grantSet(string(GrantSkills)),
	}
	if !slices.Equal(entry.Projection().Skills, []string{"skills"}) {
		t.Fatalf("projection = %+v, want the granted skills", entry.Projection())
	}
	entry.Error = "plugin hello: broken"
	if got := entry.Projection(); !slices.Equal(got.Skills, nil) {
		t.Fatalf("projection of an invalid entry = %+v, want nothing", got)
	}
	entry.Error = ""
	entry.Conflict = "the tool namespace collides"
	if got := entry.Projection(); !slices.Equal(got.Skills, nil) {
		t.Fatalf("projection of a colliding entry = %+v, want nothing", got)
	}
}

// TestVocabularyCoversBothHalves pins what a manifest may require: craft's
// channels are always there, and an application's grant is there exactly
// when the store was told about it. A blank entry is skipped rather than
// becoming a grant with an empty name, because a catalog assembled from
// capabilities that contribute no primitives collects a few.
func TestVocabularyCoversBothHalves(t *testing.T) {
	t.Parallel()
	craft := NewVocabulary()
	for _, grant := range craftGrants {
		if !craft.Has(grant) {
			t.Fatalf("craft vocabulary is missing %q", grant)
		}
	}
	if craft.Has("events.emit") {
		t.Fatal("craft vocabulary holds an application grant")
	}
	host := NewVocabulary("events.emit", " ", "")
	if !host.Has(GrantSkills) || !host.Has("events.emit") {
		t.Fatalf("host vocabulary = %v, want both halves", host.Grants())
	}
	if want := []string{
		"events.emit", "hooks.provide", "nodes.provide", "skills.provide",
		"storage.kv", "tools.provide", "ui.webview",
	}; !slices.Equal(host.Grants(), want) {
		t.Fatalf("host vocabulary = %v, want %v", host.Grants(), want)
	}
}
