package locomo

import (
	"time"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/locomo/runners"
)

// AnswerReplayRecord is the diagnostic JSONL shape emitted by
// --dump-answer-replay. It captures the exact answer input and scored
// output so verifier/audit commands can replay answer_miss cases without
// rerunning ingest or recall.
type AnswerReplayRecord struct {
	TS           time.Time           `json:"ts"`
	QID          string              `json:"qid"`
	Conversation string              `json:"conversation_id,omitempty"`
	Query        string              `json:"query"`
	AskedAt      string              `json:"asked_at,omitempty"`
	GoldAnswers  []string            `json:"gold_answers,omitempty"`
	EvidenceIDs  []string            `json:"evidence_ids,omitempty"`
	Tags         []string            `json:"tags,omitempty"`
	AnswerPrompt string              `json:"answer_prompt,omitempty"`
	AnswerBody   string              `json:"answer_body,omitempty"`
	FullPrompt   string              `json:"full_prompt,omitempty"`
	Hits         []AnswerReplayHit   `json:"hits,omitempty"`
	Outcome      AnswerReplayOutcome `json:"outcome"`
}

type AnswerReplayHit struct {
	ID          string         `json:"id,omitempty"`
	Rank        int            `json:"rank"`
	Score       float64        `json:"score,omitempty"`
	Kind        string         `json:"kind,omitempty"`
	Sources     []string       `json:"sources,omitempty"`
	EvidenceIDs []string       `json:"evidence_ids,omitempty"`
	ValidFrom   string         `json:"valid_from,omitempty"`
	Content     string         `json:"content"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

type AnswerReplayOutcome struct {
	Prediction string   `json:"prediction"`
	EM         float64  `json:"em"`
	F1         float64  `json:"f1"`
	Judge      float64  `json:"judge"`
	KHit       *float64 `json:"k_hit,omitempty"`
}

func NewAnswerReplayRecord(ts time.Time, q dataset.Question, hits []runners.Hit, outcome AnswerReplayOutcome, promptTemplate, body, fullPrompt string) AnswerReplayRecord {
	rec := AnswerReplayRecord{
		TS:           ts,
		QID:          q.ID,
		Conversation: q.ConversationID,
		Query:        q.Query,
		AskedAt:      q.AskedAt,
		GoldAnswers:  append([]string(nil), q.GoldAnswers...),
		EvidenceIDs:  append([]string(nil), q.EvidenceIDs...),
		Tags:         append([]string(nil), q.Tags...),
		AnswerPrompt: promptTemplate,
		AnswerBody:   body,
		FullPrompt:   fullPrompt,
		Outcome:      outcome,
	}
	rec.Hits = make([]AnswerReplayHit, 0, len(hits))
	for i, h := range hits {
		rec.Hits = append(rec.Hits, AnswerReplayHit{
			ID:          h.ID,
			Rank:        i + 1,
			Score:       h.Score,
			Kind:        h.Kind,
			Sources:     append([]string(nil), h.Sources...),
			EvidenceIDs: append([]string(nil), h.EvidenceIDs...),
			ValidFrom:   h.ValidFrom,
			Content:     h.Content,
			Metadata:    cloneReplayMetadata(h.Metadata),
		})
	}
	return rec
}

func cloneReplayMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
