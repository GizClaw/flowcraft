package locomo

import (
	"strings"
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
	Batch            *factDumpBatch                   `json:"batch,omitempty"`
	Error            string                           `json:"error,omitempty"`
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

type factDumpBatch struct {
	ConversationID   string         `json:"conversation_id,omitempty"`
	SessionID        string         `json:"session_id,omitempty"`
	SessionIDs       []string       `json:"session_ids,omitempty"`
	BatchNumber      int            `json:"batch_number,omitempty"`
	BatchTotal       int            `json:"batch_total,omitempty"`
	TurnCount        int            `json:"turn_count,omitempty"`
	TurnsWithText    int            `json:"turns_with_text,omitempty"`
	RecentMessages   int            `json:"recent_messages,omitempty"`
	Anchors          int            `json:"anchors,omitempty"`
	EvidenceIDs      []string       `json:"evidence_ids,omitempty"`
	SourceMessageIDs []string       `json:"source_message_ids,omitempty"`
	InputTextChars   int            `json:"input_text_chars,omitempty"`
	Turns            []factDumpTurn `json:"turns,omitempty"`
}

type factDumpTurn struct {
	ID         string `json:"id,omitempty"`
	EvidenceID string `json:"evidence_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Role       string `json:"role,omitempty"`
	Speaker    string `json:"speaker,omitempty"`
	Text       string `json:"text,omitempty"`
	Timestamp  string `json:"timestamp,omitempty"`
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
	ID               string                `json:"id,omitempty"`
	Content          string                `json:"content"`
	Kind             string                `json:"kind,omitempty"`
	Subject          string                `json:"subject,omitempty"`
	Predicate        string                `json:"predicate,omitempty"`
	Object           string                `json:"object,omitempty"`
	Location         string                `json:"location,omitempty"`
	Polarity         string                `json:"polarity,omitempty"`
	Modality         string                `json:"modality,omitempty"`
	Certainty        string                `json:"certainty,omitempty"`
	Entities         []string              `json:"entities,omitempty"`
	Participants     []string              `json:"participants,omitempty"`
	EvidenceIDs      []string              `json:"evidence_ids,omitempty"`
	SourceMessageIDs []string              `json:"source_message_ids,omitempty"`
	EvidenceText     string                `json:"evidence_text,omitempty"`
	EvidenceRefs     []factDumpEvidenceRef `json:"evidence_refs,omitempty"`
	ValidFrom        string                `json:"valid_from,omitempty"`
	ObservedAt       string                `json:"observed_at,omitempty"`
	Categories       []string              `json:"categories,omitempty"`
	Source           string                `json:"source,omitempty"`
	Confidence       float64               `json:"confidence,omitempty"`
	Episodic         bool                  `json:"episodic,omitempty"`
}

type factDumpEvidenceRef struct {
	ID            string `json:"id,omitempty"`
	MessageID     string `json:"message_id,omitempty"`
	ObservationID string `json:"observation_id,omitempty"`
	Role          string `json:"role,omitempty"`
	Text          string `json:"text,omitempty"`
	Timestamp     string `json:"timestamp,omitempty"`
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

func newV2FactsDump(ts time.Time, scope runners.Scope, req recall.SaveRequest, facts []recall.TemporalFact, diag *diagnostics.SaveDiagnostics) factDumpRecord {
	out := factDumpRecord{
		TS:     ts,
		Runner: runnerFlowcraftRecallV2,
		Scope: factDumpScope{
			RuntimeID: scope.RuntimeID,
			UserID:    scope.UserID,
			AgentID:   scope.AgentID,
		},
		Batch: batchFromSaveRequest(scope, req),
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
			Location:         f.Location,
			Polarity:         string(f.Polarity),
			Modality:         string(f.Modality),
			Certainty:        string(f.Certainty),
			Entities:         append([]string(nil), f.Entities...),
			Participants:     append([]string(nil), f.Participants...),
			SourceMessageIDs: append([]string(nil), f.SourceMessageIDs...),
			EvidenceText:     f.EvidenceText,
			Confidence:       f.Confidence,
		}
		if !f.ObservedAt.IsZero() {
			rec.ObservedAt = f.ObservedAt.UTC().Format(time.RFC3339Nano)
		}
		if f.ValidFrom != nil && !f.ValidFrom.IsZero() {
			rec.ValidFrom = f.ValidFrom.Format("2006-01-02")
		}
		for _, ref := range f.EvidenceRefs {
			if ref.ID != "" {
				rec.EvidenceIDs = append(rec.EvidenceIDs, ref.ID)
			}
			rec.EvidenceRefs = append(rec.EvidenceRefs, factDumpEvidenceRef{
				ID:            ref.ID,
				MessageID:     ref.MessageID,
				ObservationID: ref.ObservationID,
				Role:          ref.Role,
				Text:          ref.Text,
				Timestamp:     factDumpTime(ref.Timestamp),
			})
		}
		out.Facts = append(out.Facts, rec)
	}
	return out
}

func factDumpTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func newV2IngestErrorDump(ts time.Time, scope runners.Scope, convID string, batch turnBatch, batchNumber, batchTotal int, err error) factDumpRecord {
	out := factDumpRecord{
		TS:     ts,
		Type:   "ingest_error",
		Runner: runnerFlowcraftRecallV2,
		Scope: factDumpScope{
			RuntimeID: scope.RuntimeID,
			UserID:    scope.UserID,
			AgentID:   scope.AgentID,
		},
		Batch: batchFromRawTurns(scope, convID, batch.rawTurns, len(batch.recentRawTurns), batchNumber, batchTotal),
		Facts: []factDumpFact{},
	}
	if err != nil {
		out.Error = err.Error()
	}
	return out
}

func batchFromSaveRequest(scope runners.Scope, req recall.SaveRequest) *factDumpBatch {
	if len(req.Turns) == 0 {
		return nil
	}
	b := &factDumpBatch{
		ConversationID: conversationIDFromRunnerScope(scope),
		RecentMessages: len(req.RecentMessages),
		Anchors:        len(req.ExistingFactsAnchor),
	}
	sessionSeen := map[string]struct{}{}
	evidenceSeen := map[string]struct{}{}
	sourceSeen := map[string]struct{}{}
	for _, turn := range req.Turns {
		b.TurnCount++
		if strings.TrimSpace(turn.Text) != "" {
			b.TurnsWithText++
			b.InputTextChars += len(turn.Text)
		}
		if session := strings.TrimSpace(turn.SessionID); session != "" {
			if _, ok := sessionSeen[session]; !ok {
				sessionSeen[session] = struct{}{}
				b.SessionIDs = append(b.SessionIDs, session)
			}
		}
		if id := strings.TrimSpace(turn.EvidenceID); id != "" {
			if _, ok := evidenceSeen[id]; !ok {
				evidenceSeen[id] = struct{}{}
				b.EvidenceIDs = append(b.EvidenceIDs, id)
			}
		}
		if id := strings.TrimSpace(turn.ID); id != "" {
			if _, ok := sourceSeen[id]; !ok {
				sourceSeen[id] = struct{}{}
				b.SourceMessageIDs = append(b.SourceMessageIDs, id)
			}
		}
		if text := strings.TrimSpace(turn.Text); text != "" {
			b.Turns = append(b.Turns, factDumpTurn{
				ID:         strings.TrimSpace(turn.ID),
				EvidenceID: strings.TrimSpace(turn.EvidenceID),
				SessionID:  strings.TrimSpace(turn.SessionID),
				Role:       strings.TrimSpace(turn.Role),
				Speaker:    strings.TrimSpace(turn.Speaker),
				Text:       text,
				Timestamp:  factDumpTime(turn.Time),
			})
		}
	}
	if len(b.SessionIDs) == 1 {
		b.SessionID = b.SessionIDs[0]
		b.SessionIDs = nil
	}
	return b
}

func batchFromRawTurns(scope runners.Scope, convID string, turns []runners.RawTurn, recentCount, batchNumber, batchTotal int) *factDumpBatch {
	b := &factDumpBatch{
		ConversationID: convID,
		BatchNumber:    batchNumber,
		BatchTotal:     batchTotal,
		RecentMessages: recentCount,
	}
	if b.ConversationID == "" {
		b.ConversationID = conversationIDFromRunnerScope(scope)
	}
	sessionSeen := map[string]struct{}{}
	evidenceSeen := map[string]struct{}{}
	for _, turn := range turns {
		b.TurnCount++
		if strings.TrimSpace(turn.Content) != "" {
			b.TurnsWithText++
			b.InputTextChars += len(turn.Content)
		}
		if session := strings.TrimSpace(turn.SessionID); session != "" {
			if _, ok := sessionSeen[session]; !ok {
				sessionSeen[session] = struct{}{}
				b.SessionIDs = append(b.SessionIDs, session)
			}
		}
		if id := strings.TrimSpace(turn.EvidenceID); id != "" {
			if _, ok := evidenceSeen[id]; !ok {
				evidenceSeen[id] = struct{}{}
				b.EvidenceIDs = append(b.EvidenceIDs, id)
			}
		}
		if text := strings.TrimSpace(turn.Content); text != "" {
			b.Turns = append(b.Turns, factDumpTurn{
				ID:         strings.TrimSpace(turn.EvidenceID),
				EvidenceID: strings.TrimSpace(turn.EvidenceID),
				SessionID:  strings.TrimSpace(turn.SessionID),
				Role:       strings.TrimSpace(turn.Role),
				Text:       text,
			})
		}
	}
	if len(b.SessionIDs) == 1 {
		b.SessionID = b.SessionIDs[0]
		b.SessionIDs = nil
	}
	return b
}

func conversationIDFromRunnerScope(scope runners.Scope) string {
	if idx := strings.LastIndex(scope.UserID, "::"); idx >= 0 {
		return scope.UserID[idx+2:]
	}
	return scope.UserID
}
