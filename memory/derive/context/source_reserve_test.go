package context

import (
	stdctx "context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestSourceBackedReservePackerAppendsEntitySourceMessages(t *testing.T) {
	resolver := fakeSourceMessageResolver{
		"conv\x00m-reserve": {ID: "m-reserve", ConversationID: "conv", Message: model.NewTextMessage(model.RoleUser, "reserve evidence")},
	}
	packer := SourceBackedReservePacker{
		MaxReserveMessages:   2,
		MinQueryTokens:       2,
		MinDerivedHits:       1,
		MinReserveCandidates: 1,
		UseEntityFactRefs:    true,
		GateOn:               []ReserveDerivedHitKind{ReserveDerivedSummary},
		ReserveMetadata:      map[string]any{"retrieval_origin": "entity_fact_expanded"},
	}

	out, err := packer.PackContext(stdctx.Background(), derive.ContextPackInput{
		Query:          "reserve bridge evidence",
		SourceMessages: resolver,
		Items: []derive.ContextItem{{
			Kind: derive.ContextItemRecentMessage,
			Text: "user: baseline",
			Message: &sourcemessage.Message{
				ID:             "m-base",
				ConversationID: "conv",
				Message:        model.NewTextMessage(model.RoleUser, "baseline"),
			},
			Retrieval: &retrieval.Hit{Score: 1},
		}},
		SummaryHits: []derive.SummaryNodeSearchHit{{
			Retrieval: retrieval.Hit{Score: 0.9},
			Node:      viewrecent.SummaryNode{ID: "summary-1", Summary: "summary"},
		}},
		EntityHits: []derive.EntityFactSearchHit{{
			Retrieval: retrieval.Hit{Score: 0.8},
			Fact: viewentityfact.Fact{
				ID:         "fact-1",
				FactText:   "entity fact",
				Confidence: 0.8,
				SourceRefs: []views.SourceRef{messageRef("conv", "m-reserve")},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("Items len = %d, want baseline + reserve", len(out.Items))
	}
	if out.Items[0].Message.ID != "m-base" {
		t.Fatalf("first item = %+v, want baseline preserved first", out.Items[0].Message)
	}
	reserve := out.Items[1]
	if reserve.Message == nil || reserve.Message.ID != "m-reserve" {
		t.Fatalf("reserve item = %+v, want hydrated source message", reserve)
	}
	if got := reserve.Message.Metadata["source_backed_reserve"]; got != true {
		t.Fatalf("source_backed_reserve metadata = %v, want true", got)
	}
	if got := reserve.Message.Metadata["retrieval_origin"]; got != "entity_fact_expanded" {
		t.Fatalf("retrieval_origin metadata = %v, want entity_fact_expanded", got)
	}
}

func TestSourceBackedReservePackerSkipsWhenGateFails(t *testing.T) {
	packer := SourceBackedReservePacker{
		MaxReserveMessages:   2,
		MinQueryTokens:       3,
		MinDerivedHits:       2,
		MinReserveCandidates: 1,
		UseEntityFactRefs:    true,
		GateOn:               []ReserveDerivedHitKind{ReserveDerivedSummary},
	}
	out, err := packer.PackContext(stdctx.Background(), derive.ContextPackInput{
		Query:          "short query",
		SourceMessages: fakeSourceMessageResolver{"conv\x00m-reserve": {ID: "m-reserve", ConversationID: "conv", Message: model.NewTextMessage(model.RoleUser, "reserve evidence")}},
		Items:          []derive.ContextItem{{Kind: derive.ContextItemRecentMessage, Text: "user: baseline"}},
		SummaryHits: []derive.SummaryNodeSearchHit{{
			Node: viewrecent.SummaryNode{ID: "summary-1", Summary: "summary"},
		}},
		EntityHits: []derive.EntityFactSearchHit{{
			Fact: viewentityfact.Fact{SourceRefs: []views.SourceRef{messageRef("conv", "m-reserve")}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("Items len = %d, want no reserve when gate fails", len(out.Items))
	}
}

func TestSourceBackedReservePackerDedupesBaselineMessages(t *testing.T) {
	packer := SourceBackedReservePacker{
		MaxReserveMessages:   2,
		MinReserveCandidates: 1,
		UseEntityFactRefs:    true,
	}
	msg := sourcemessage.Message{ID: "m-1", ConversationID: "conv", Message: model.NewTextMessage(model.RoleUser, "same evidence")}
	out, err := packer.PackContext(stdctx.Background(), derive.ContextPackInput{
		Query:          "same evidence",
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
	if len(out.Items) != 1 {
		t.Fatalf("Items len = %d, want duplicate reserve skipped", len(out.Items))
	}
}

type fakeSourceMessageResolver map[string]sourcemessage.Message

func (r fakeSourceMessageResolver) GetSourceMessage(_ stdctx.Context, conversationID, messageID string) (sourcemessage.Message, bool, error) {
	msg, ok := r[conversationID+"\x00"+messageID]
	return msg, ok, nil
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
