package locomo

import (
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
	recallv1 "github.com/GizClaw/flowcraft/sdk/recall"
)

type factDumpRecord struct {
	TS               time.Time                        `json:"ts"`
	Type             string                           `json:"type,omitempty"`
	Runner           string                           `json:"runner,omitempty"`
	Scope            factDumpScope                    `json:"scope"`
	ExtractCount     int                              `json:"extract_count,omitempty"`
	ExtractTokens    *diagnostics.ExtractorTokenUsage `json:"extract_tokens,omitempty"`
	AvgExtractTokens *factDumpAvgTokens               `json:"avg_extract_tokens,omitempty"`
	Facts            []factDumpFact                   `json:"facts"`
}

type factDumpTokenStats struct {
	Extracts int
	Tokens   diagnostics.TokenUsage
}

type factDumpAvgTokens struct {
	InputTokens       float64 `json:"input_tokens,omitempty"`
	CachedInputTokens float64 `json:"cached_input_tokens,omitempty"`
	OutputTokens      float64 `json:"output_tokens,omitempty"`
	TotalTokens       float64 `json:"total_tokens,omitempty"`
	CostMicros        float64 `json:"cost_micros,omitempty"`
}

type factDumpScope struct {
	RuntimeID string `json:"runtime_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

func (s *factDumpTokenStats) Add(usage diagnostics.ExtractorTokenUsage) {
	if usage.Calls <= 0 {
		return
	}
	s.Extracts++
	s.Tokens.InputTokens += usage.InputTokens
	s.Tokens.CachedInputTokens += usage.CachedInputTokens
	s.Tokens.OutputTokens += usage.OutputTokens
	s.Tokens.TotalTokens += usage.TotalTokens
	s.Tokens.CostMicros += usage.CostMicros
	if s.Tokens.Model == "" {
		s.Tokens.Model = usage.Model
	}
}

func newV2FactsDumpSummary(ts time.Time, stats factDumpTokenStats) factDumpRecord {
	rec := factDumpRecord{
		TS:           ts,
		Type:         "extract_token_summary",
		Runner:       runnerFlowcraftRecallV2,
		ExtractCount: stats.Extracts,
		Facts:        []factDumpFact{},
	}
	if stats.Extracts <= 0 {
		return rec
	}
	total := diagnostics.ExtractorTokenUsage{
		Calls:      stats.Extracts,
		TokenUsage: stats.Tokens,
	}
	rec.ExtractTokens = &total
	rec.AvgExtractTokens = &factDumpAvgTokens{
		InputTokens:       float64(stats.Tokens.InputTokens) / float64(stats.Extracts),
		CachedInputTokens: float64(stats.Tokens.CachedInputTokens) / float64(stats.Extracts),
		OutputTokens:      float64(stats.Tokens.OutputTokens) / float64(stats.Extracts),
		TotalTokens:       float64(stats.Tokens.TotalTokens) / float64(stats.Extracts),
		CostMicros:        float64(stats.Tokens.CostMicros) / float64(stats.Extracts),
	}
	return rec
}

type factDumpFact struct {
	ID               string   `json:"id,omitempty"`
	Content          string   `json:"content"`
	Kind             string   `json:"kind,omitempty"`
	Subject          string   `json:"subject,omitempty"`
	Predicate        string   `json:"predicate,omitempty"`
	Object           string   `json:"object,omitempty"`
	Polarity         string   `json:"polarity,omitempty"`
	Modality         string   `json:"modality,omitempty"`
	Certainty        string   `json:"certainty,omitempty"`
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

func newV2FactsDump(ts time.Time, scope runners.Scope, facts []recall.TemporalFact, diag *diagnostics.SaveDiagnostics) factDumpRecord {
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
	if diag != nil && diag.ExtractorTokenUsage.Calls > 0 {
		usage := diag.ExtractorTokenUsage
		out.ExtractTokens = &usage
	}
	for _, f := range facts {
		rec := factDumpFact{
			ID:               f.ID,
			Content:          f.Content,
			Kind:             string(f.Kind),
			Subject:          f.Subject,
			Predicate:        f.Predicate,
			Object:           f.Object,
			Polarity:         string(f.Polarity),
			Modality:         string(f.Modality),
			Certainty:        string(f.Certainty),
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
