package pipeline

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/memory/retrieval"
)

// stubLinkResolver records its calls and returns a canned id list.
type stubLinkResolver struct {
	gotNs       string
	gotEntities []string
	gotCap      int
	ids         []string
	err         error
	callCount   int
}

func (s *stubLinkResolver) ResolveLinks(_ context.Context, ns string, ents []string, cap int) ([]string, error) {
	s.callCount++
	s.gotNs = ns
	s.gotEntities = ents
	s.gotCap = cap
	if s.err != nil {
		return nil, s.err
	}
	return s.ids, nil
}

func TestEntityLinkLookup_NoOpOnNilResolver(t *testing.T) {
	st := &State{Namespace: "ns1", QueryEntities: []string{"alice"}}
	stage := EntityLinkLookup{Resolver: nil}
	if err := stage.Run(context.Background(), st); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(st.CandidateEntityIDs) != 0 {
		t.Fatalf("expected no candidates, got %v", st.CandidateEntityIDs)
	}
}

func TestEntityLinkLookup_NoOpOnEmptyEntities(t *testing.T) {
	res := &stubLinkResolver{ids: []string{"should-not-appear"}}
	st := &State{Namespace: "ns1"}
	stage := EntityLinkLookup{Resolver: res, PerEntityCap: 10}
	if err := stage.Run(context.Background(), st); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.callCount != 0 {
		t.Fatalf("resolver called %d times despite empty entities", res.callCount)
	}
	if len(st.CandidateEntityIDs) != 0 {
		t.Fatalf("candidates leaked: %v", st.CandidateEntityIDs)
	}
}

func TestEntityLinkLookup_PassesNamespaceAndCap(t *testing.T) {
	res := &stubLinkResolver{ids: []string{"e1", "e2"}}
	st := &State{
		Namespace:     "ltm_rt1__u_u1",
		QueryEntities: []string{"alice", "bob"},
	}
	stage := EntityLinkLookup{Resolver: res, PerEntityCap: 7}
	if err := stage.Run(context.Background(), st); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.gotNs != "ltm_rt1__u_u1" {
		t.Fatalf("gotNs=%q", res.gotNs)
	}
	if res.gotCap != 7 {
		t.Fatalf("gotCap=%d; want 7", res.gotCap)
	}
	if !reflect.DeepEqual(res.gotEntities, []string{"alice", "bob"}) {
		t.Fatalf("gotEntities=%v", res.gotEntities)
	}
	if !reflect.DeepEqual(st.CandidateEntityIDs, []string{"e1", "e2"}) {
		t.Fatalf("candidates=%v", st.CandidateEntityIDs)
	}
}

func TestEntityLinkLookup_PropagatesError(t *testing.T) {
	res := &stubLinkResolver{err: errors.New("boom")}
	st := &State{Namespace: "ns1", QueryEntities: []string{"alice"}}
	stage := EntityLinkLookup{Resolver: res}
	if err := stage.Run(context.Background(), st); err == nil {
		t.Fatal("expected error, got nil")
	}
	if st.CandidateEntityIDs != nil {
		t.Fatalf("candidates leaked: %v", st.CandidateEntityIDs)
	}
}

// docGetterIndex is a minimal retrieval.Index that also implements
// DocGetter, used to drive ModeEntityLink in unit tests without
// pulling in the real in-memory backend.
type docGetterIndex struct {
	docs map[string]retrieval.Doc
}

func (i *docGetterIndex) Upsert(context.Context, string, []retrieval.Doc) error { return nil }
func (i *docGetterIndex) Delete(context.Context, string, []string) error        { return nil }
func (i *docGetterIndex) Search(context.Context, string, retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	return &retrieval.SearchResponse{}, nil
}
func (i *docGetterIndex) List(context.Context, string, retrieval.ListRequest) (*retrieval.ListResponse, error) {
	return &retrieval.ListResponse{}, nil
}
func (i *docGetterIndex) Capabilities() retrieval.Capabilities { return retrieval.Capabilities{} }
func (i *docGetterIndex) Close() error                         { return nil }
func (i *docGetterIndex) Get(_ context.Context, _ string, id string) (retrieval.Doc, bool, error) {
	d, ok := i.docs[id]
	return d, ok, nil
}

func TestRunEntityLinkRecall_MaterializesHitsScoredByRank(t *testing.T) {
	idx := &docGetterIndex{docs: map[string]retrieval.Doc{
		"e1": {ID: "e1", Content: "alice loves coffee"},
		"e2": {ID: "e2", Content: "alice plays tennis"},
		"e4": {ID: "e4", Content: "alice fourth"},
	}}
	st := &State{
		Index:              idx,
		Namespace:          "ns1",
		Request:            &retrieval.SearchRequest{},
		CandidateEntityIDs: []string{"e1", "e2", "e3-missing", "e4"},
	}
	hits, err := runEntityLinkRecall(context.Background(), st, retrieval.SearchRequest{}, RetrieveSpec{Mode: ModeEntityLink, TopK: 10})
	if err != nil {
		t.Fatalf("runEntityLinkRecall: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits (1 skipped as missing); got %d", len(hits))
	}
	// Rank assignment: e1 -> 1/1, e2 -> 1/2, e4 -> 1/4 (e3 missing
	// did NOT decrement the rank — staleness must not promote the
	// next id, otherwise rank-vote becomes non-monotonic w.r.t.
	// resolver ordering).
	wantScores := []float64{1.0, 1.0 / 2.0, 1.0 / 4.0}
	for i, h := range hits {
		if h.Score != wantScores[i] {
			t.Fatalf("hit[%d].Score = %v; want %v (doc=%s)", i, h.Score, wantScores[i], h.Doc.ID)
		}
		if h.Scores["entity_link"] != wantScores[i] {
			t.Fatalf("hit[%d].Scores[entity_link] = %v; want %v", i, h.Scores["entity_link"], wantScores[i])
		}
	}
}

func TestRunEntityLinkRecall_HonoursTopK(t *testing.T) {
	idx := &docGetterIndex{docs: map[string]retrieval.Doc{
		"e1": {ID: "e1"}, "e2": {ID: "e2"}, "e3": {ID: "e3"},
	}}
	st := &State{
		Index:              idx,
		Namespace:          "ns1",
		Request:            &retrieval.SearchRequest{},
		CandidateEntityIDs: []string{"e1", "e2", "e3"},
	}
	hits, err := runEntityLinkRecall(context.Background(), st, retrieval.SearchRequest{}, RetrieveSpec{Mode: ModeEntityLink, TopK: 2})
	if err != nil {
		t.Fatalf("runEntityLinkRecall: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("TopK=2 should cap hits; got %d", len(hits))
	}
}

func TestRunEntityLinkRecall_EmptyCandidates(t *testing.T) {
	idx := &docGetterIndex{}
	st := &State{
		Index:     idx,
		Namespace: "ns1",
		Request:   &retrieval.SearchRequest{},
	}
	hits, err := runEntityLinkRecall(context.Background(), st, retrieval.SearchRequest{}, RetrieveSpec{Mode: ModeEntityLink})
	if err != nil || len(hits) != 0 {
		t.Fatalf("empty candidates should produce no hits; got hits=%v err=%v", hits, err)
	}
}

// TestRunEntityLinkRecall_HonoursRequestFilter pins the fix for #148:
// docs the resolver references but the request's Filter excludes
// (tombstoned / expired / cross-agent rows) must NOT leak into the
// fused result. The resolver-returned rank order is preserved for
// the docs that DO pass — i.e. the filter-rejected slots do not
// shift the next id up.
func TestRunEntityLinkRecall_HonoursRequestFilter(t *testing.T) {
	idx := &docGetterIndex{docs: map[string]retrieval.Doc{
		"e1": {ID: "e1", Metadata: map[string]any{"tombstone": true}},
		"e2": {ID: "e2", Metadata: map[string]any{"tombstone": false}},
		"e3": {ID: "e3", Metadata: map[string]any{"tombstone": true}},
		"e4": {ID: "e4", Metadata: map[string]any{"tombstone": false}},
	}}
	st := &State{
		Index:              idx,
		Namespace:          "ns1",
		Request:            &retrieval.SearchRequest{},
		CandidateEntityIDs: []string{"e1", "e2", "e3", "e4"},
	}
	req := retrieval.SearchRequest{
		Filter: retrieval.Filter{Neq: map[string]any{"tombstone": true}},
	}
	hits, err := runEntityLinkRecall(context.Background(), st, req, RetrieveSpec{Mode: ModeEntityLink, TopK: 10})
	if err != nil {
		t.Fatalf("runEntityLinkRecall: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits after filter (e1/e3 tombstoned); got %d hits=%+v", len(hits), hits)
	}
	if hits[0].Doc.ID != "e2" || hits[1].Doc.ID != "e4" {
		t.Fatalf("hit ordering = [%s, %s]; want [e2, e4]", hits[0].Doc.ID, hits[1].Doc.ID)
	}
	// Rank invariant: filter-rejected slots must NOT promote the
	// next surviving id. e2 was at rank 2 (score 1/2), e4 at rank 4
	// (score 1/4). If we had used hits[*]'s position instead, e4
	// would have scored 1/2 — wrong.
	if hits[0].Score != 1.0/2.0 {
		t.Fatalf("hit[e2].Score = %v; want 1/2 (rank preserved across filtered slots)", hits[0].Score)
	}
	if hits[1].Score != 1.0/4.0 {
		t.Fatalf("hit[e4].Score = %v; want 1/4 (rank preserved across filtered slots)", hits[1].Score)
	}
}

// nonGetterIndex omits DocGetter so we can assert the defensive nil
// fallback in runEntityLinkRecall.
type nonGetterIndex struct{}

func (*nonGetterIndex) Upsert(context.Context, string, []retrieval.Doc) error { return nil }
func (*nonGetterIndex) Delete(context.Context, string, []string) error        { return nil }
func (*nonGetterIndex) Search(context.Context, string, retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	return &retrieval.SearchResponse{}, nil
}
func (*nonGetterIndex) List(context.Context, string, retrieval.ListRequest) (*retrieval.ListResponse, error) {
	return &retrieval.ListResponse{}, nil
}
func (*nonGetterIndex) Capabilities() retrieval.Capabilities { return retrieval.Capabilities{} }
func (*nonGetterIndex) Close() error                         { return nil }

func TestRunEntityLinkRecall_SilentOnNonDocGetterBackend(t *testing.T) {
	st := &State{
		Index:              &nonGetterIndex{},
		Namespace:          "ns1",
		Request:            &retrieval.SearchRequest{},
		CandidateEntityIDs: []string{"e1"},
	}
	hits, err := runEntityLinkRecall(context.Background(), st, retrieval.SearchRequest{}, RetrieveSpec{Mode: ModeEntityLink})
	if err != nil {
		t.Fatalf("err=%v; want nil (graceful degrade)", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits=%v; want empty (lane should stay silent)", hits)
	}
}
