package version

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
)

// mockDataStore is a test double for version.DataStore, backed by an in-memory slice.
type mockDataStore struct {
	mu       sync.RWMutex
	versions []*model.GraphVersion
}

func (m *mockDataStore) ListGraphVersions(_ context.Context, agentID string) ([]*model.GraphVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*model.GraphVersion
	for _, v := range m.versions {
		if v.AgentID == agentID {
			result = append(result, v)
		}
	}
	return result, nil
}

func (m *mockDataStore) GetGraphVersion(_ context.Context, agentID string, version int) (*model.GraphVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, v := range m.versions {
		if v.AgentID == agentID && v.Version == version {
			return v, nil
		}
	}
	return nil, &notFoundErr{fmt.Sprintf("%s/v%d", agentID, version)}
}

func (m *mockDataStore) SaveGraphVersion(_ context.Context, gv *model.GraphVersion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, v := range m.versions {
		if v.ID == gv.ID || (v.AgentID == gv.AgentID && v.Version == gv.Version) {
			m.versions[i] = gv
			return nil
		}
	}
	m.versions = append(m.versions, gv)
	return nil
}

func (m *mockDataStore) GetLatestPublishedVersion(_ context.Context, agentID string) (*model.GraphVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var best *model.GraphVersion
	for _, v := range m.versions {
		if v.AgentID == agentID && v.PublishedAt != nil {
			if best == nil || v.Version > best.Version {
				best = v
			}
		}
	}
	if best == nil {
		return nil, &notFoundErr{agentID}
	}
	return best, nil
}

func (m *mockDataStore) PublishGraphVersion(_ context.Context, agentID string, def *model.GraphDefinition, desc string) (*model.GraphVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	maxVer := 0
	for _, v := range m.versions {
		if v.AgentID == agentID && v.Version > maxVer {
			maxVer = v.Version
		}
	}
	now := time.Now()
	gv := &model.GraphVersion{
		ID:          fmt.Sprintf("v-%s-%d", agentID, maxVer+1),
		AgentID:     agentID,
		Version:     maxVer + 1,
		GraphDef:    def,
		Description: desc,
		Checksum:    ComputeChecksum(def),
		PublishedAt: &now,
		CreatedAt:   now,
	}
	m.versions = append(m.versions, gv)
	return gv, nil
}

func (m *mockDataStore) UpdateVersionLock(_ context.Context, agentID string, expectedChecksum string, newDef *model.GraphDefinition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.versions {
		if v.AgentID == agentID && v.PublishedAt == nil && v.Checksum == expectedChecksum {
			v.GraphDef = newDef
			v.Checksum = ComputeChecksum(newDef)
			return nil
		}
	}
	return fmt.Errorf("checksum mismatch or no draft found")
}

type notFoundErr struct{ id string }

func (e *notFoundErr) Error() string { return "not found: " + e.id }

func TestVersionStore_SaveDraft_GetDraft(t *testing.T) {
	ds := &mockDataStore{}
	vs := NewVersionStore(ds)
	ctx := context.Background()

	def := &model.GraphDefinition{Name: "test", Entry: "start", Nodes: []model.NodeDefinition{{ID: "start", Type: "llm"}}}
	gv := &model.GraphVersion{
		AgentID:  "app-1",
		Version:  1,
		GraphDef: def,
	}
	if err := vs.SaveDraft(ctx, gv); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	draft, err := vs.GetDraft(ctx, "app-1")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if draft.GraphDef.Name != "test" {
		t.Fatalf("expected 'test', got %q", draft.GraphDef.Name)
	}
	if draft.Checksum == "" {
		t.Fatal("expected checksum to be set")
	}
}

func TestVersionStore_Publish(t *testing.T) {
	ds := &mockDataStore{}
	vs := NewVersionStore(ds)
	ctx := context.Background()

	def := &model.GraphDefinition{Name: "test", Entry: "start", Nodes: []model.NodeDefinition{{ID: "start", Type: "llm"}}}
	gv := &model.GraphVersion{
		AgentID:  "app-1",
		Version:  1,
		GraphDef: def,
	}
	_ = vs.SaveDraft(ctx, gv)

	v, err := vs.Publish(ctx, "app-1", 1, "initial release")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if v.Version != 1 {
		t.Fatalf("expected version 1, got %d", v.Version)
	}
	if v.PublishedAt == nil {
		t.Fatal("expected PublishedAt to be set after publish")
	}

	draft, err := vs.GetDraft(ctx, "app-1")
	if err != nil {
		t.Fatalf("expected next draft to be auto-created after publish: %v", err)
	}
	if draft.Version != 2 {
		t.Fatalf("expected auto-created draft version 2, got %d", draft.Version)
	}
	if draft.PublishedAt != nil {
		t.Fatal("auto-created draft should not be published")
	}
}

func TestVersionStore_Rollback(t *testing.T) {
	ds := &mockDataStore{}
	vs := NewVersionStore(ds)
	ctx := context.Background()

	def1 := &model.GraphDefinition{Name: "v1", Entry: "start", Nodes: []model.NodeDefinition{{ID: "start", Type: "llm"}}}
	def2 := &model.GraphDefinition{Name: "v2", Entry: "start", Nodes: []model.NodeDefinition{{ID: "start", Type: "template"}}}

	gv1 := &model.GraphVersion{AgentID: "app-1", Version: 1, GraphDef: def1}
	_ = vs.SaveDraft(ctx, gv1)
	_, _ = vs.Publish(ctx, "app-1", 1, "v1")

	// Publish auto-created V2 draft; overwrite it with different content.
	gv2 := &model.GraphVersion{AgentID: "app-1", Version: 2, GraphDef: def2}
	_ = vs.SaveDraft(ctx, gv2)
	_, _ = vs.Publish(ctx, "app-1", 2, "v2")

	// After publishing V2, auto-draft V3 exists. Rollback creates a new published version.
	rolled, err := vs.Rollback(ctx, "app-1", 1)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// Rollback creates version = max+1. V1(pub), V2(pub), V3(draft) → rollback = V4.
	if rolled.Version != 4 {
		t.Fatalf("expected version 4 (rollback creates new version), got %d", rolled.Version)
	}
	if rolled.GraphDef.Name != "v1" {
		t.Fatalf("expected definition from v1, got %q", rolled.GraphDef.Name)
	}
}

func TestVersionStore_Diff(t *testing.T) {
	ds := &mockDataStore{}
	vs := NewVersionStore(ds)
	ctx := context.Background()

	def1 := &model.GraphDefinition{Name: "v1", Entry: "a", Nodes: []model.NodeDefinition{{ID: "a", Type: "llm"}}}
	def2 := &model.GraphDefinition{Name: "v2", Entry: "a", Nodes: []model.NodeDefinition{{ID: "a", Type: "llm"}, {ID: "b", Type: "template"}}}

	gv1 := &model.GraphVersion{AgentID: "app-1", Version: 1, GraphDef: def1}
	_ = vs.SaveDraft(ctx, gv1)
	_, _ = vs.Publish(ctx, "app-1", 1, "v1")

	gv2 := &model.GraphVersion{AgentID: "app-1", Version: 2, GraphDef: def2}
	_ = vs.SaveDraft(ctx, gv2)
	_, _ = vs.Publish(ctx, "app-1", 2, "v2")

	diff, err := vs.Diff(ctx, "app-1", 1, 2)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(diff.NodesAdded) != 1 || diff.NodesAdded[0].ID != "b" {
		t.Fatalf("expected node b added, got %v", diff.NodesAdded)
	}
}

func TestVersionStore_ListVersions(t *testing.T) {
	ds := &mockDataStore{}
	vs := NewVersionStore(ds)
	ctx := context.Background()

	def := &model.GraphDefinition{Name: "test", Entry: "start", Nodes: []model.NodeDefinition{{ID: "start", Type: "llm"}}}
	gv := &model.GraphVersion{AgentID: "app-1", Version: 1, GraphDef: def}
	_ = vs.SaveDraft(ctx, gv)
	_, _ = vs.Publish(ctx, "app-1", 1, "v1")

	versions, err := vs.ListVersions(ctx, "app-1")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	// V1 (published) + V2 (auto-created draft)
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions (1 published + 1 auto-draft), got %d", len(versions))
	}
}

func TestVersionStore_GetPublished(t *testing.T) {
	ds := &mockDataStore{}
	vs := NewVersionStore(ds)
	ctx := context.Background()

	def := &model.GraphDefinition{Name: "test", Entry: "start", Nodes: []model.NodeDefinition{{ID: "start", Type: "llm"}}}
	gv := &model.GraphVersion{AgentID: "app-1", Version: 1, GraphDef: def}
	_ = vs.SaveDraft(ctx, gv)
	_, _ = vs.Publish(ctx, "app-1", 1, "v1")

	pub, err := vs.GetPublished(ctx, "app-1")
	if err != nil {
		t.Fatalf("GetPublished: %v", err)
	}
	if pub.Version != 1 {
		t.Fatalf("expected version 1, got %d", pub.Version)
	}
}

func TestVersionStore_Rollback_NotFound(t *testing.T) {
	ds := &mockDataStore{}
	vs := NewVersionStore(ds)
	ctx := context.Background()

	_, err := vs.Rollback(ctx, "app-1", 999)
	if err == nil {
		t.Fatal("expected error for nonexistent version")
	}
}

func TestVersionStore_Diff_NotFound(t *testing.T) {
	ds := &mockDataStore{}
	vs := NewVersionStore(ds)
	ctx := context.Background()

	_, err := vs.Diff(ctx, "app-1", 1, 2)
	if err == nil {
		t.Fatal("expected error for nonexistent version")
	}
}

func TestVersionStore_ConcurrentAccess(t *testing.T) {
	ds := &mockDataStore{}
	vs := NewVersionStore(ds)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			def := &model.GraphDefinition{
				Name:  "concurrent",
				Entry: "start",
				Nodes: []model.NodeDefinition{{ID: "start", Type: "llm"}},
			}
			gv := &model.GraphVersion{
				AgentID:  "app-c",
				Version:  n + 1,
				GraphDef: def,
			}
			_ = vs.SaveDraft(ctx, gv)
			_, _ = vs.GetDraft(ctx, "app-c")
		}(i)
	}
	wg.Wait()

	draft, err := vs.GetDraft(ctx, "app-c")
	if err != nil {
		t.Fatalf("GetDraft after concurrent access: %v", err)
	}
	if draft.GraphDef.Name != "concurrent" {
		t.Fatalf("expected 'concurrent', got %q", draft.GraphDef.Name)
	}
}
