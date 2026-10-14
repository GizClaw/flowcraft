package projection

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// newIdentityStore builds a map-valued store whose deltas can be compacted on
// demand, so one test can exercise the append and the compaction write paths.
func newIdentityStore(t *testing.T, kvStore storage.Store, producerVersion string, maxSegments int) *Store[map[string]bool, map[string]bool] {
	t.Helper()
	store, err := NewTypedStore(kvStore, "test", TypedOptions[map[string]bool, map[string]bool]{
		ProducerVersion: producerVersion,
		Thresholds:      Thresholds{MaxSegments: maxSegments, MaxDeltaBytes: 1 << 20},
		Apply: func(base *map[string]bool, delta map[string]bool) error {
			for key, value := range delta {
				(*base)[key] = value
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func identityOf(t *testing.T, store *Store[map[string]bool, map[string]bool], scope corememory.Scope, name string) component.ProducerIdentity {
	t.Helper()
	identity, found, err := store.ProducerIdentity(context.Background(), scope, name)
	if err != nil || !found {
		t.Fatalf("producer identity = %#v, %v, %v", identity, found, err)
	}
	return identity
}

// A rebuild stamps the state it publishes; an appended delta carries the stamp
// forward rather than refreshing it, and so does a compaction -- rewriting the
// storage shape must not make state a newer producer wrote look current.
func TestProducerVersionSurvivesDeltasAndCompaction(t *testing.T) {
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	store := newIdentityStore(t, kvStore, "lane-v1", 1)
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := store.FullRebuild(ctx, scope, "index", map[string]bool{"base": true}, "source"); err != nil {
		t.Fatal(err)
	}
	if version := identityOf(t, store, scope, "index").Version; version != "lane-v1" {
		t.Fatalf("rebuilt version = %q", version)
	}
	// The first delta fits the segment budget, the second exceeds it and
	// compacts, which is the write path that republishes the base.
	for _, delta := range []map[string]bool{{"first": true}, {"second": true}} {
		if _, err := store.ApplyDelta(ctx, scope, "index", delta, "revision", "input"); err != nil {
			t.Fatal(err)
		}
	}
	manifest, found, err := store.loadManifestLocked(ctx, scope, "index")
	if err != nil || !found {
		t.Fatalf("manifest = %#v, %v, %v", manifest, found, err)
	}
	if len(manifest.Segments) != 0 {
		t.Fatalf("expected a compacted manifest, got %d segments", len(manifest.Segments))
	}
	identity := identityOf(t, store, scope, "index")
	if identity.Version != "lane-v1" {
		t.Fatalf("compacted version = %q, want the version that wrote the state", identity.Version)
	}
	if identity.SourceDigest == "" || identity.BuildDigest == "" {
		t.Fatalf("compacted identity lost its digests = %#v", identity)
	}
}

// State written before lanes stamped a version stays readable, and no later
// write launders it: the version it was read back with is empty -- unknown,
// not current -- until a rebuild stamps it.
func TestUnstampedStateReadsAsUnknownUntilRebuilt(t *testing.T) {
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	legacy := newIdentityStore(t, kvStore, "", 1)
	if err := legacy.FullRebuild(ctx, scope, "index", map[string]bool{"base": true}, "source"); err != nil {
		t.Fatal(err)
	}
	raw, err := kvStore.Get(ctx, legacy.activePath(scope, "index"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "producer_version") {
		t.Fatalf("an unstamped manifest marshaled a version: %s", raw)
	}
	current := newIdentityStore(t, kvStore, "lane-v2", 1)
	if version := identityOf(t, current, scope, "index").Version; version != "" {
		t.Fatalf("unstamped state reported version %q", version)
	}
	if _, err := current.ApplyDelta(ctx, scope, "index", map[string]bool{"later": true}, "revision", "input"); err != nil {
		t.Fatal(err)
	}
	if version := identityOf(t, current, scope, "index").Version; version != "" {
		t.Fatalf("a delta apply stamped unstamped state as %q", version)
	}
	if err := current.FullRebuild(ctx, scope, "index", map[string]bool{"rebuilt": true}, "source"); err != nil {
		t.Fatal(err)
	}
	if version := identityOf(t, current, scope, "index").Version; version != "lane-v2" {
		t.Fatalf("rebuilt version = %q", version)
	}
}

// legacyManifest is the manifest a build without the producer stamp writes:
// the same fields in the same order, minus that one. Such a build drops the
// field when it unmarshals, so the identity it recomputes is the digest of
// exactly these bytes.
type legacyManifest struct {
	SchemaVersion    int              `json:"schema_version"`
	StorageVersion   string           `json:"storage_version"`
	Projection       string           `json:"projection"`
	Scope            corememory.Scope `json:"scope"`
	Base             *BaseRef         `json:"base,omitempty"`
	Segments         []SegmentRef     `json:"segments"`
	BaseSourceDigest string           `json:"base_source_digest"`
	SourceDigest     string           `json:"source_digest"`
	BuildDigest      string           `json:"build_digest"`
	BaseLastApplied  *ReplayRef       `json:"base_last_applied,omitempty"`
	LastApplied      *ReplayRef       `json:"last_applied,omitempty"`
	Identity         string           `json:"identity"`
}

// The producer stamp records who wrote the state rather than what the state
// is, so it stays out of the manifest identity: a build that predates the
// field recomputes the identity of a manifest this build wrote, which is what
// keeps a rollback -- or a mixed-version deployment over one workspace -- from
// reading every lane the newer build touched as corrupt.
func TestManifestIdentitySurvivesAReaderWithoutTheStamp(t *testing.T) {
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	store := newIdentityStore(t, kvStore, "lane-v1", 1)
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := store.FullRebuild(ctx, scope, "index", map[string]bool{"base": true}, "source"); err != nil {
		t.Fatal(err)
	}
	manifest, found, err := store.loadManifestLocked(ctx, scope, "index")
	if err != nil || !found {
		t.Fatalf("manifest = %#v, %v, %v", manifest, found, err)
	}
	if manifest.ProducerVersion != "lane-v1" {
		t.Fatalf("rebuilt manifest is not stamped: %#v", manifest)
	}
	encoded, err := json.Marshal(legacyManifest{
		SchemaVersion: manifest.SchemaVersion, StorageVersion: manifest.StorageVersion,
		Projection: manifest.Projection, Scope: manifest.Scope, Base: manifest.Base,
		Segments: manifest.Segments, BaseSourceDigest: manifest.BaseSourceDigest,
		SourceDigest: manifest.SourceDigest, BuildDigest: manifest.BuildDigest,
		BaseLastApplied: manifest.BaseLastApplied, LastApplied: manifest.LastApplied,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := chainDigest("manifest", string(encoded)); got != manifest.Identity {
		t.Fatalf("identity a reader without the stamp recomputes = %q, stored %q", got, manifest.Identity)
	}
}
