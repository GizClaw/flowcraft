package locomo

import (
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/recall"
	recallv1 "github.com/GizClaw/flowcraft/sdk/recall"
)

type factDumpRecord struct {
	TS     time.Time      `json:"ts"`
	Runner string         `json:"runner,omitempty"`
	Scope  factDumpScope  `json:"scope"`
	Facts  []factDumpFact `json:"facts"`
}

type factDumpScope struct {
	RuntimeID string `json:"runtime_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

type factDumpFact struct {
	ID               string   `json:"id,omitempty"`
	Content          string   `json:"content"`
	Kind             string   `json:"kind,omitempty"`
	Subject          string   `json:"subject,omitempty"`
	Predicate        string   `json:"predicate,omitempty"`
	Object           string   `json:"object,omitempty"`
	Entities         []string `json:"entities,omitempty"`
	EvidenceIDs      []string `json:"evidence_ids,omitempty"`
	SourceMessageIDs []string `json:"source_message_ids,omitempty"`
	EvidenceText     string   `json:"evidence_text,omitempty"`
	ValidFrom        string   `json:"valid_from,omitempty"`
	Categories       []string `json:"categories,omitempty"`
	Source           string   `json:"source,omitempty"`
	Confidence       float64  `json:"confidence,omitempty"`
	Episodic         bool     `json:"episodic,omitempty"`
}

func newV1FactsDump(ts time.Time, scope recallv1.Scope, facts []recallv1.ExtractedFact) factDumpRecord {
	out := factDumpRecord{
		TS:     ts,
		Runner: runnerFlowcraftRecallV1,
		Scope: factDumpScope{
			RuntimeID: scope.RuntimeID,
			UserID:    scope.UserID,
			AgentID:   scope.AgentID,
		},
		Facts: make([]factDumpFact, 0, len(facts)),
	}
	for _, f := range facts {
		out.Facts = append(out.Facts, factDumpFact{
			Content:    f.Content,
			Subject:    f.Subject,
			Predicate:  f.Predicate,
			Entities:   append([]string(nil), f.Entities...),
			Categories: append([]string(nil), f.Categories...),
			Source:     f.Source,
			Confidence: f.Confidence,
			Episodic:   f.Episodic,
		})
	}
	return out
}

func newV2FactsDump(ts time.Time, scope runners.Scope, facts []recall.TemporalFact) factDumpRecord {
	out := factDumpRecord{
		TS:     ts,
		Runner: runnerFlowcraftRecallV2,
		Scope: factDumpScope{
			RuntimeID: scope.RuntimeID,
			UserID:    scope.UserID,
			AgentID:   scope.AgentID,
		},
		Facts: make([]factDumpFact, 0, len(facts)),
	}
	for _, f := range facts {
		rec := factDumpFact{
			ID:               f.ID,
			Content:          f.Content,
			Kind:             string(f.Kind),
			Subject:          f.Subject,
			Predicate:        f.Predicate,
			Object:           f.Object,
			Entities:         append([]string(nil), f.Entities...),
			SourceMessageIDs: append([]string(nil), f.SourceMessageIDs...),
			EvidenceText:     f.EvidenceText,
			Confidence:       f.Confidence,
		}
		if f.ValidFrom != nil && !f.ValidFrom.IsZero() {
			rec.ValidFrom = f.ValidFrom.Format("2006-01-02")
		}
		for _, ref := range f.EvidenceRefs {
			if ref.ID != "" {
				rec.EvidenceIDs = append(rec.EvidenceIDs, ref.ID)
			}
		}
		out.Facts = append(out.Facts, rec)
	}
	return out
}
