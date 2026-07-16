package bbh

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/dgraph-io/badger/v4"
)

func TestInspectorValidationAndEmptyWorkspace(t *testing.T) {
	if _, err := NewInspector(nil); err == nil {
		t.Fatal("nil workspace should fail")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, badgerDir), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewInspector(rootedWorkspace(t, dir)); err == nil {
		t.Fatal("badger file should fail")
	}

	empty := t.TempDir()
	emptyWS := rootedWorkspace(t, empty)
	in, err := NewInspector(emptyWS)
	if err != nil {
		t.Fatalf("NewInspector empty: %v", err)
	}
	got, err := in.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect empty: %v", err)
	}
	if got.Root != emptyWS.Root() || got.BadgerExists || got.BleveExists || got.HNSWExists {
		t.Fatalf("unexpected empty inspection: %+v", got)
	}
	if len(got.Namespaces) != 0 || got.TotalDocs != 0 {
		t.Fatalf("unexpected empty namespaces: %+v", got)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := in.Inspect(cancelled); err == nil {
		t.Fatal("Inspect with cancelled context should fail")
	}
	if _, err := in.InspectNamespace(cancelled, "ns"); err == nil {
		t.Fatal("InspectNamespace with cancelled context should fail")
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Inspect(context.Background()); err == nil {
		t.Fatal("inspect after close should fail")
	}
}

func TestInspectorReportsNamespaces(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openInternalIndex(t, dir, WithConfig(Config{
		HNSW: HNSWConfig{FlushInterval: Duration{Duration: time.Hour}},
	}))
	if err := idx.Upsert(ctx, "runtime/user/agent-a", []retrieval.Doc{
		{ID: "a", Content: "alpha", Vector: []float32{1, 0}, Timestamp: time.Unix(1, 0).UTC()},
		{ID: "b", Content: "beta", Timestamp: time.Unix(2, 0).UTC()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, "runtime/user/agent-b", []retrieval.Doc{
		{ID: "c", Content: "gamma", Vector: []float32{0, 1}, Timestamp: time.Unix(3, 0).UTC()},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Search(ctx, "empty/shard", retrieval.SearchRequest{QueryText: "nothing"}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	in, err := NewInspector(rootedWorkspace(t, dir))
	if err != nil {
		t.Fatalf("NewInspector: %v", err)
	}
	defer func() {
		if err := in.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	got, err := in.Inspect(ctx)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !got.BadgerExists || !got.BleveExists || !got.HNSWExists {
		t.Fatalf("expected all storage roots, got %+v", got)
	}
	if got.TotalDocs != 3 || got.TotalVectorDocs != 2 {
		t.Fatalf("totals = docs %d vectors %d, want 3/2", got.TotalDocs, got.TotalVectorDocs)
	}
	if got.PhysicalNamespaceCount != 3 || got.DocNamespaceCount != 2 {
		t.Fatalf("namespace counts = physical %d doc %d, want 3/2", got.PhysicalNamespaceCount, got.DocNamespaceCount)
	}

	a := mustInspectNamespace(t, in, "runtime/user/agent-a")
	if a.DocCount != 2 || a.VectorDocCount != 1 {
		t.Fatalf("agent-a counts = %+v", a)
	}
	if !a.SourceBadger || !a.SourceBleve || !a.SourceHNSW || !a.BleveExists || !a.HNSWExists {
		t.Fatalf("agent-a sources = %+v", a)
	}
	if a.BadgerBytes <= 0 || a.BleveSizeBytes <= 0 || a.HNSWSizeBytes <= 0 {
		t.Fatalf("agent-a sizes = %+v", a)
	}

	empty := mustInspectNamespace(t, in, "empty/shard")
	if empty.DocCount != 0 || !empty.SourceBleve || !empty.Empty {
		t.Fatalf("empty shard = %+v", empty)
	}

	missing, err := in.InspectNamespace(ctx, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if missing.Namespace != "missing" || missing.Token != safeToken("missing") || missing.DocCount != 0 {
		t.Fatalf("missing namespace = %+v", missing)
	}
	if !missing.Empty {
		t.Fatalf("missing namespace Empty = false, want true: %+v", missing)
	}
}

func TestInspectorDocumentsOfflineOnlyContract(t *testing.T) {
	idx := openInternalIndex(t, t.TempDir(), WithConfig(Config{
		HNSW: HNSWConfig{FlushInterval: Duration{Duration: time.Hour}},
	}))
	defer idx.Close()
	if _, err := NewInspector(rootedWorkspace(t, idx.root)); err == nil {
		t.Fatal("NewInspector should fail while writable Index has Badger open")
	}
}

func TestInspectNamespaceIsScoped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openInternalIndex(t, dir, WithConfig(Config{
		HNSW: HNSWConfig{FlushInterval: Duration{Duration: time.Hour}},
	}))
	if err := idx.Upsert(ctx, "target", []retrieval.Doc{
		{ID: "ok", Content: "alpha", Vector: []float32{1}, Timestamp: time.Unix(1, 0).UTC()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := badger.Open(badger.DefaultOptions(filepath.Join(dir, badgerDir)).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(txn *badger.Txn) error {
		return txn.Set(docKey("broken", "bad-json"), []byte("{"))
	})
	if closeErr := db.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}

	in, err := NewInspector(rootedWorkspace(t, dir))
	if err != nil {
		t.Fatalf("NewInspector: %v", err)
	}
	defer in.Close()
	got, err := in.InspectNamespace(ctx, "target")
	if err != nil {
		t.Fatalf("InspectNamespace target: %v", err)
	}
	if got.DocCount != 1 || got.VectorDocCount != 1 {
		t.Fatalf("target namespace = %+v, want one vector doc", got)
	}
	if _, err := in.Inspect(ctx); err == nil {
		t.Fatal("full Inspect should still report unrelated corrupt doc")
	}
}

func TestInspectorHandlesInvalidPhysicalToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, bleveDir, "not@base64"), 0o755); err != nil {
		t.Fatal(err)
	}
	in, err := NewInspector(rootedWorkspace(t, dir))
	if err != nil {
		t.Fatalf("NewInspector: %v", err)
	}
	defer func() {
		if err := in.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	got, err := in.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Namespaces) != 1 {
		t.Fatalf("namespaces = %+v", got.Namespaces)
	}
	ns := got.Namespaces[0]
	if ns.Namespace != "not@base64" || ns.DecodeError == "" || !ns.SourceBleve {
		t.Fatalf("invalid token namespace = %+v", ns)
	}
}

func mustInspectNamespace(t *testing.T, in *Inspector, namespace string) NamespaceInspection {
	t.Helper()
	got, err := in.InspectNamespace(context.Background(), namespace)
	if err != nil {
		t.Fatalf("InspectNamespace %s: %v", namespace, err)
	}
	return got
}
