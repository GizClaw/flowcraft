package context

import (
	stdctx "context"
	"sort"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestSourceEvidencePackerHydratesSummaryAndEntitySourceMessages(t *testing.T) {
	resolver := fakeSourceMessageResolver{
		"conv\x00m-summary": {ID: "m-summary", ConversationID: "conv", Message: model.NewTextMessage(model.RoleUser, "summary evidence")},
		"conv\x00m-entity":  {ID: "m-entity", ConversationID: "conv", Message: model.NewTextMessage(model.RoleUser, "entity evidence")},
	}
	packer := SourceEvidencePacker{
		SourceOnly:            true,
		MaxSummaryMessages:    1,
		MaxEntityFactMessages: 1,
		MaxSourceRefsPerHit:   1,
		UseSummaryRefs:        true,
		UseEntityFactRefs:     true,
		OriginMetadataKey:     "retrieval_origin",
		OriginMetadataValues: SourceEvidenceOriginValues{
			Summary:    "summary_expanded",
			EntityFact: "entity_fact_expanded",
		},
	}

	out, err := packer.PackContext(stdctx.Background(), derive.ContextPackInput{
		Scope:          views.Scope{ConversationID: "conv"},
		SourceMessages: resolver,
		SummaryHits: []derive.SummaryNodeSearchHit{{
			Retrieval: retrieval.Hit{Score: 0.9},
			Node: viewrecent.SummaryNode{
				ID:         "summary-1",
				Summary:    "summary",
				SourceRefs: []views.SourceRef{messageRef("conv", "m-summary")},
			},
		}},
		EntityHits: []derive.EntityFactSearchHit{{
			Retrieval: retrieval.Hit{Score: 0.8},
			Fact: viewentityfact.Fact{
				ID:         "fact-1",
				FactText:   "entity fact",
				Confidence: 0.8,
				SourceRefs: []views.SourceRef{messageRef("conv", "m-entity")},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(out.Items), 2; got != want {
		t.Fatalf("Items len = %d, want %d", got, want)
	}
	if got := out.Items[0].Message.Metadata["retrieval_origin"]; got != "summary_expanded" {
		t.Fatalf("summary origin = %v, want summary_expanded", got)
	}
	if got := out.Items[1].Message.Metadata["retrieval_origin"]; got != "entity_fact_expanded" {
		t.Fatalf("entity origin = %v, want entity_fact_expanded", got)
	}
	if got := out.Items[0].Message.Metadata[SourceEvidenceOriginMetadataKey]; got != string(SourceEvidenceOriginSummary) {
		t.Fatalf("source evidence origin = %v, want summary", got)
	}
}

func TestSourceEvidencePackerDedupesBaselineMessages(t *testing.T) {
	packer := SourceEvidencePacker{
		SourceOnly:            false,
		MaxEntityFactMessages: 1,
		UseEntityFactRefs:     true,
	}
	msg := sourcemessage.Message{ID: "m-1", ConversationID: "conv", Message: model.NewTextMessage(model.RoleUser, "same evidence")}
	out, err := packer.PackContext(stdctx.Background(), derive.ContextPackInput{
		SourceMessages: fakeSourceMessageResolver{"conv\x00m-1": msg},
		Items: []derive.ContextItem{{
			Kind:    derive.ContextItemRecentMessage,
			Text:    "user: same evidence",
			Message: &msg,
		}},
		EntityHits: []derive.EntityFactSearchHit{{
			Fact: viewentityfact.Fact{SourceRefs: []views.SourceRef{messageRef("conv", "m-1")}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(out.Items), 1; got != want {
		t.Fatalf("Items len = %d, want duplicate base message replaced in-place", got)
	}
	if out.Items[0].Message == nil || out.Items[0].Message.Metadata[SourceEvidenceOriginMetadataKey] != string(SourceEvidenceOriginEntityFact) {
		t.Fatalf("item metadata = %+v, want entity source evidence", out.Items[0].Message)
	}
}

func TestSourceEvidencePackerSkipsWhenGateFailsAndRespectsBudget(t *testing.T) {
	packer := SourceEvidencePacker{
		SourceOnly:            true,
		MaxEntityFactMessages: 1,
		MinQueryTokens:        3,
		UseEntityFactRefs:     true,
	}
	out, err := packer.PackContext(stdctx.Background(), derive.ContextPackInput{
		Query:          "short query",
		SourceMessages: fakeSourceMessageResolver{"conv\x00m-1": {ID: "m-1", ConversationID: "conv", Message: model.NewTextMessage(model.RoleUser, "reserve evidence")}},
		EntityHits: []derive.EntityFactSearchHit{{
			Fact: viewentityfact.Fact{SourceRefs: []views.SourceRef{messageRef("conv", "m-1")}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 0 {
		t.Fatalf("Items len = %d, want no evidence when gate fails", len(out.Items))
	}

	packer.MinQueryTokens = 0
	out, err = packer.PackContext(stdctx.Background(), derive.ContextPackInput{
		Query: "long enough query",
		SourceMessages: fakeSourceMessageResolver{
			"conv\x00m-1": {ID: "m-1", ConversationID: "conv", Message: model.NewTextMessage(model.RoleUser, "first")},
			"conv\x00m-2": {ID: "m-2", ConversationID: "conv", Message: model.NewTextMessage(model.RoleUser, "second")},
		},
		EntityHits: []derive.EntityFactSearchHit{{
			Fact: viewentityfact.Fact{SourceRefs: []views.SourceRef{messageRef("conv", "m-1"), messageRef("conv", "m-2")}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("Items len = %d, want budgeted one evidence item", len(out.Items))
	}
}

func TestSourceEvidencePackerGraphCandidatesUseEntityHitSeeds(t *testing.T) {
	resolver := fakeSourceMessageResolver{
		"conv\x00m-graph": {ID: "m-graph", ConversationID: "conv", Message: model.NewTextMessage(model.RoleUser, "graph evidence")},
	}
	graph := &fakeGraphSourceResolver{result: viewentityfact.GraphExpansionResult{
		Candidates: []viewentityfact.GraphSourceCandidate{{
			SourceRef:     messageRef("conv", "m-graph"),
			Origin:        viewentityfact.GraphOriginFact,
			FactIDs:       []viewentityfact.FactID{"fact-seed"},
			SeedEntityIDs: []viewentityfact.EntityID{"ent-ada"},
			Paths:         []string{"entities:ent-ada -> fact:fact-seed"},
			Score:         0.7,
		}},
	}}
	seedFact := viewentityfact.Fact{
		ID:              "fact-seed",
		SubjectEntityID: "ent-ada",
		ObjectEntityIDs: []viewentityfact.EntityID{"ent-tea"},
		RelationType:    viewentityfact.RelationPreference,
		Confidence:      0.9,
		SourceRefs:      []views.SourceRef{messageRef("conv", "m-graph")},
	}
	packer := SourceEvidencePacker{
		SourceOnly:        true,
		MaxGraphMessages:  1,
		UseGraphSources:   true,
		GraphMaxSeedFacts: 1,
		OriginMetadataKey: "retrieval_origin",
		OriginMetadataValues: SourceEvidenceOriginValues{
			Graph: "graph_fact_expanded",
		},
	}

	out, err := packer.PackContext(stdctx.Background(), derive.ContextPackInput{
		Scope:              views.Scope{ConversationID: "conv"},
		SourceMessages:     resolver,
		EntityGraphSources: graph,
		EntityHits: []derive.EntityFactSearchHit{{
			Retrieval: retrieval.Hit{Score: 1},
			Fact:      seedFact,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.seeds) != 1 || graph.seeds[0].Fact.ID != "fact-seed" {
		t.Fatalf("graph seeds = %+v, want top entity hit fact seed", graph.seeds)
	}
	if got, want := len(out.Items), 1; got != want {
		t.Fatalf("Items len = %d, want graph evidence", got)
	}
	item := out.Items[0]
	if got := item.Message.Metadata["retrieval_origin"]; got != "graph_fact_expanded" {
		t.Fatalf("graph origin = %v, want graph_fact_expanded", got)
	}
	if got := item.Message.Metadata[GraphFactIDsMetadataKey]; !reflectStringSlice(got, []string{"fact-seed"}) {
		t.Fatalf("graph fact metadata = %v, want fact-seed", got)
	}
}

func TestSourceEvidencePackerGraphSeedsSkipNonGraphableFacts(t *testing.T) {
	resolver := fakeSourceMessageResolver{
		"conv\x00m-graph": {ID: "m-graph", ConversationID: "conv", Message: model.NewTextMessage(model.RoleUser, "graph evidence")},
	}
	graph := &fakeGraphSourceResolver{result: viewentityfact.GraphExpansionResult{
		Candidates: []viewentityfact.GraphSourceCandidate{{
			SourceRef:     messageRef("conv", "m-graph"),
			Origin:        viewentityfact.GraphOriginFact,
			FactIDs:       []viewentityfact.FactID{"fact-graphable"},
			SeedEntityIDs: []viewentityfact.EntityID{"ent-ada"},
			Paths:         []string{"entities:ent-ada -> fact:fact-graphable"},
			Score:         0.7,
		}},
	}}
	nonGraphableFact := viewentityfact.Fact{
		ID:              "fact-unary",
		SubjectEntityID: "ent-ada",
		RelationType:    viewentityfact.RelationPreference,
		Confidence:      0.99,
		SourceRefs:      []views.SourceRef{messageRef("conv", "m-unary")},
		Metadata:        map[string]any{viewentityfact.FactGraphableMetadataKey: false},
	}
	graphableFact := viewentityfact.Fact{
		ID:              "fact-graphable",
		SubjectEntityID: "ent-ada",
		ObjectEntityIDs: []viewentityfact.EntityID{"ent-tea"},
		RelationType:    viewentityfact.RelationPreference,
		Confidence:      0.8,
		SourceRefs:      []views.SourceRef{messageRef("conv", "m-graph")},
	}
	packer := SourceEvidencePacker{
		SourceOnly:        true,
		MaxGraphMessages:  1,
		UseGraphSources:   true,
		GraphMaxSeedFacts: 2,
	}

	out, err := packer.PackContext(stdctx.Background(), derive.ContextPackInput{
		Scope:              views.Scope{ConversationID: "conv"},
		SourceMessages:     resolver,
		EntityGraphSources: graph,
		EntityHits: []derive.EntityFactSearchHit{
			{Retrieval: retrieval.Hit{Score: 1}, Fact: nonGraphableFact},
			{Retrieval: retrieval.Hit{Score: 0.8}, Fact: graphableFact},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.seeds) != 1 || graph.seeds[0].Fact.ID != "fact-graphable" {
		t.Fatalf("graph seeds = %+v, want only graphable entity hit fact seed", graph.seeds)
	}
	if len(out.Items) != 1 {
		t.Fatalf("Items len = %d, want graph evidence from remaining graphable seed", len(out.Items))
	}
}

func TestSourceEvidencePackerAddsSourceNeighborhoodFromSelectedAnchors(t *testing.T) {
	resolver := fakeSourceMessageResolver{
		"conv\x00m-1": {ID: "m-1", ConversationID: "conv", Seq: 1, Message: model.NewTextMessage(model.RoleUser, "before")},
		"conv\x00m-2": {ID: "m-2", ConversationID: "conv", Seq: 2, Message: model.NewTextMessage(model.RoleUser, "anchor")},
		"conv\x00m-3": {ID: "m-3", ConversationID: "conv", Seq: 3, Message: model.NewTextMessage(model.RoleUser, "after")},
	}
	packer := SourceEvidencePacker{
		SourceOnly:              true,
		MaxSummaryMessages:      1,
		MaxNeighborhoodMessages: 2,
		UseSummaryRefs:          true,
		UseNeighborhood:         true,
		NeighborhoodBefore:      1,
		NeighborhoodAfter:       1,
		NeighborhoodAnchors:     []SourceEvidenceOrigin{SourceEvidenceOriginSummary},
		OriginMetadataKey:       "retrieval_origin",
		OriginMetadataValues: SourceEvidenceOriginValues{
			Summary:      "summary_expanded",
			Neighborhood: "source_neighborhood_expanded",
		},
	}
	out, err := packer.PackContext(stdctx.Background(), derive.ContextPackInput{
		Scope:          views.Scope{ConversationID: "conv"},
		SourceMessages: resolver,
		SummaryHits: []derive.SummaryNodeSearchHit{{
			Node: viewrecent.SummaryNode{
				ID:         "summary-1",
				Summary:    "summary",
				SourceRefs: []views.SourceRef{messageRef("conv", "m-2")},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(out.Items), 3; got != want {
		t.Fatalf("Items len = %d, want %d", got, want)
	}
	if got := out.Items[1].Message.Metadata["retrieval_origin"]; got != "source_neighborhood_expanded" {
		t.Fatalf("neighbor origin = %v, want source_neighborhood_expanded", got)
	}
	if ids := []string{out.Items[0].Message.ID, out.Items[1].Message.ID, out.Items[2].Message.ID}; strings.Join(ids, ",") != "m-2,m-1,m-3" {
		t.Fatalf("item ids = %v, want anchor then ordered neighbors", ids)
	}
}

type fakeSourceMessageResolver map[string]sourcemessage.Message

func (r fakeSourceMessageResolver) GetSourceMessage(_ stdctx.Context, conversationID, messageID string) (sourcemessage.Message, bool, error) {
	msg, ok := r[conversationID+"\x00"+messageID]
	return msg, ok, nil
}

func (r fakeSourceMessageResolver) GetSourceMessageNeighbors(_ stdctx.Context, conversationID, messageID string, before, after int) ([]sourcemessage.Message, error) {
	var messages []sourcemessage.Message
	for key, msg := range r {
		if strings.HasPrefix(key, conversationID+"\x00") {
			messages = append(messages, msg)
		}
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].Seq < messages[j].Seq })
	anchor := -1
	for i, msg := range messages {
		if msg.ID == messageID {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return nil, nil
	}
	var out []sourcemessage.Message
	maxDistance := before
	if after > maxDistance {
		maxDistance = after
	}
	for distance := 1; distance <= maxDistance; distance++ {
		if distance <= before {
			if idx := anchor - distance; idx >= 0 {
				out = append(out, messages[idx])
			}
		}
		if distance <= after {
			if idx := anchor + distance; idx < len(messages) {
				out = append(out, messages[idx])
			}
		}
	}
	return out, nil
}

type fakeGraphSourceResolver struct {
	seeds  []viewentityfact.GraphSeedFact
	result viewentityfact.GraphExpansionResult
}

func (r *fakeGraphSourceResolver) ExpandGraphSources(_ stdctx.Context, _ views.Scope, seedFacts []viewentityfact.GraphSeedFact, _ viewentityfact.GraphExpansionOptions) (viewentityfact.GraphExpansionResult, error) {
	r.seeds = append([]viewentityfact.GraphSeedFact(nil), seedFacts...)
	return r.result, nil
}

func messageRef(conversationID, messageID string) views.SourceRef {
	return views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: conversationID,
			MessageID:      messageID,
		},
	}
}

func reflectStringSlice(value any, want []string) bool {
	got, ok := value.([]string)
	if !ok || len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
