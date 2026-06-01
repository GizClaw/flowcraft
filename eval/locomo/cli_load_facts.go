package locomo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/recall"
)

type v2FactSaver interface {
	SaveFacts(ctx context.Context, scope runners.Scope, facts []recall.TemporalFact) (saveCount int, saveLatency time.Duration, err error)
}

type v2FactTurnSaver interface {
	SaveFactsWithTurns(ctx context.Context, scope runners.Scope, facts []recall.TemporalFact, turns []recall.TurnContext, observedAt time.Time) (saveCount int, saveLatency time.Duration, err error)
}

type factsLoadSummary struct {
	Records   int
	Facts     int
	SaveCalls int
	Latencies []time.Duration
}

const factsLoadChunkSize = 512

func loadV2FactsDump(ctx context.Context, r runners.Runner, path string) (factsLoadSummary, error) {
	var summary factsLoadSummary
	path = strings.TrimSpace(path)
	if path == "" {
		return summary, nil
	}
	saver, ok := r.(v2FactSaver)
	if !ok {
		return summary, fmt.Errorf("--load-facts requires a flowcraft-recall-v2 runner that can save extracted facts")
	}
	f, err := os.Open(path)
	if err != nil {
		return summary, err
	}
	defer f.Close()

	type scopeFacts struct {
		scope      runners.Scope
		facts      []recall.TemporalFact
		turns      []recall.TurnContext
		turnSeen   map[string]struct{}
		observedAt time.Time
	}
	groups := map[string]*scopeFacts{}
	dec := json.NewDecoder(f)
	for {
		var rec factDumpRecord
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return summary, fmt.Errorf("decode facts dump %s: %w", path, err)
		}
		if rec.Type != "" {
			continue
		}
		if rec.Runner != "" && rec.Runner != runnerFlowcraftRecallV2 {
			continue
		}
		if err := validateFactDumpEvidenceIDs(rec); err != nil {
			return summary, err
		}
		facts, err := temporalFactsFromDump(rec.Facts)
		if err != nil {
			return summary, err
		}
		if len(facts) == 0 {
			continue
		}
		turns, observedAt, err := turnContextsFromDumpRecord(rec)
		if err != nil {
			return summary, err
		}
		scope := scopeFromFactsDump(rec)
		summary.Records++
		groupKey := factDumpScopeKey(scope)
		group := groups[groupKey]
		if group == nil {
			group = &scopeFacts{scope: scope, turnSeen: map[string]struct{}{}}
			groups[groupKey] = group
		}
		group.facts = append(group.facts, facts...)
		if !observedAt.IsZero() && (group.observedAt.IsZero() || observedAt.Before(group.observedAt)) {
			group.observedAt = observedAt
		}
		for _, turn := range turns {
			key := factDumpTurnKey(turn)
			if key == "" {
				continue
			}
			if _, dup := group.turnSeen[key]; dup {
				continue
			}
			group.turnSeen[key] = struct{}{}
			group.turns = append(group.turns, turn)
		}
	}
	turnSaver, hasTurnSaver := r.(v2FactTurnSaver)
	for _, group := range groups {
		for start := 0; start < len(group.facts); start += factsLoadChunkSize {
			end := min(start+factsLoadChunkSize, len(group.facts))
			var (
				n       int
				latency time.Duration
				err     error
			)
			if hasTurnSaver && start == 0 && len(group.turns) > 0 {
				n, latency, err = turnSaver.SaveFactsWithTurns(ctx, group.scope, group.facts[start:end], group.turns, group.observedAt)
			} else {
				n, latency, err = saver.SaveFacts(ctx, group.scope, group.facts[start:end])
			}
			if err != nil {
				return summary, fmt.Errorf("load facts for scope %s/%s/%s: %w", group.scope.RuntimeID, group.scope.UserID, group.scope.AgentID, err)
			}
			summary.Facts += n
			summary.SaveCalls++
			summary.Latencies = append(summary.Latencies, latency)
		}
	}
	return summary, nil
}

func factDumpScopeKey(scope runners.Scope) string {
	return scope.RuntimeID + "\x00" + scope.UserID + "\x00" + scope.AgentID
}

func scopeFromFactsDump(rec factDumpRecord) runners.Scope {
	scope := runners.Scope{
		RuntimeID: rec.Scope.RuntimeID,
		UserID:    rec.Scope.UserID,
		AgentID:   rec.Scope.AgentID,
	}
	if scope.RuntimeID == "" {
		scope.RuntimeID = "locomo"
	}
	if scope.AgentID == "" {
		scope.AgentID = "agent-bench"
	}
	if scope.UserID == "" {
		scope.UserID = "u-bench"
		if rec.Batch != nil && rec.Batch.ConversationID != "" {
			scope.UserID += "::" + rec.Batch.ConversationID
		}
	}
	return scope
}

func temporalFactsFromDump(in []factDumpFact) ([]recall.TemporalFact, error) {
	out := make([]recall.TemporalFact, 0, len(in))
	for _, f := range in {
		content := strings.TrimSpace(f.Content)
		if content == "" {
			continue
		}
		fact := recall.TemporalFact{
			Kind:             recall.FactKind(strings.TrimSpace(f.Kind)),
			Content:          content,
			Subject:          strings.TrimSpace(f.Subject),
			Predicate:        strings.TrimSpace(f.Predicate),
			Object:           strings.TrimSpace(f.Object),
			Location:         strings.TrimSpace(f.Location),
			Polarity:         recall.Polarity(strings.TrimSpace(f.Polarity)),
			Modality:         recall.Modality(strings.TrimSpace(f.Modality)),
			Certainty:        recall.Certainty(strings.TrimSpace(f.Certainty)),
			Entities:         append([]string(nil), f.Entities...),
			Participants:     append([]string(nil), f.Participants...),
			SourceMessageIDs: append([]string(nil), f.SourceMessageIDs...),
			EvidenceText:     strings.TrimSpace(f.EvidenceText),
			Confidence:       f.Confidence,
		}
		if observedAt := strings.TrimSpace(f.ObservedAt); observedAt != "" {
			t, err := parseFactDumpDate(observedAt)
			if err != nil {
				return nil, fmt.Errorf("parse fact %q observed_at %q: %w", content, observedAt, err)
			}
			fact.ObservedAt = t
		}
		if fact.Kind == "" {
			fact.Kind = recall.FactNote
		}
		if fact.Polarity == "" {
			fact.Polarity = recall.PolarityAffirmed
		}
		if fact.Modality == "" {
			fact.Modality = recall.ModalityActual
		}
		if fact.Certainty == "" {
			fact.Certainty = recall.CertaintyExplicit
		}
		if vf := strings.TrimSpace(f.ValidFrom); vf != "" {
			t, err := parseFactDumpDate(vf)
			if err != nil {
				return nil, fmt.Errorf("parse fact %q valid_from %q: %w", content, vf, err)
			}
			fact.ValidFrom = &t
		}
		fact.EvidenceRefs = evidenceRefsFromDumpFact(f)
		out = append(out, fact)
	}
	return out, nil
}

func turnContextsFromDumpRecord(rec factDumpRecord) ([]recall.TurnContext, time.Time, error) {
	if rec.Batch == nil || len(rec.Batch.Turns) == 0 {
		return nil, time.Time{}, nil
	}
	out := make([]recall.TurnContext, 0, len(rec.Batch.Turns))
	var observedAt time.Time
	for _, t := range rec.Batch.Turns {
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		id := strings.TrimSpace(t.ID)
		evidenceID := strings.TrimSpace(t.EvidenceID)
		if id == "" {
			id = evidenceID
		}
		if evidenceID == "" {
			evidenceID = id
		}
		var ts time.Time
		if raw := strings.TrimSpace(t.Timestamp); raw != "" {
			parsed, err := parseFactDumpDate(raw)
			if err != nil {
				return nil, time.Time{}, fmt.Errorf("parse dump turn %q timestamp %q: %w", firstNonEmpty(id, evidenceID), raw, err)
			}
			ts = parsed
			if observedAt.IsZero() || ts.Before(observedAt) {
				observedAt = ts
			}
		}
		out = append(out, recall.TurnContext{
			ID:         id,
			EvidenceID: evidenceID,
			SessionID:  strings.TrimSpace(t.SessionID),
			Role:       strings.TrimSpace(t.Role),
			Speaker:    strings.TrimSpace(t.Speaker),
			Time:       ts,
			Text:       text,
		})
	}
	return out, observedAt, nil
}

func factDumpTurnKey(turn recall.TurnContext) string {
	if id := strings.TrimSpace(turn.EvidenceID); id != "" {
		return id
	}
	if id := strings.TrimSpace(turn.ID); id != "" {
		return id
	}
	text := strings.TrimSpace(turn.Text)
	if text == "" {
		return ""
	}
	return "text:" + text
}

func evidenceRefsFromDumpFact(f factDumpFact) []recall.EvidenceRef {
	if len(f.EvidenceRefs) > 0 {
		out := make([]recall.EvidenceRef, 0, len(f.EvidenceRefs))
		for _, in := range f.EvidenceRefs {
			ref := recall.EvidenceRef{
				ID:            strings.TrimSpace(in.ID),
				MessageID:     strings.TrimSpace(in.MessageID),
				ObservationID: strings.TrimSpace(in.ObservationID),
				Role:          strings.TrimSpace(in.Role),
				Text:          strings.TrimSpace(in.Text),
			}
			if in.Timestamp != "" {
				if ts, err := parseFactDumpDate(in.Timestamp); err == nil {
					ref.Timestamp = ts
				}
			}
			out = append(out, ref)
		}
		return out
	}
	ids := append([]string(nil), f.EvidenceIDs...)
	if len(ids) == 0 && strings.TrimSpace(f.EvidenceText) == "" {
		return nil
	}
	if len(ids) == 0 {
		return []recall.EvidenceRef{{Text: strings.TrimSpace(f.EvidenceText)}}
	}
	out := make([]recall.EvidenceRef, 0, len(ids))
	for i, id := range ids {
		ref := recall.EvidenceRef{ID: strings.TrimSpace(id)}
		if i < len(f.SourceMessageIDs) {
			ref.MessageID = strings.TrimSpace(f.SourceMessageIDs[i])
		}
		if i == 0 {
			ref.Text = strings.TrimSpace(f.EvidenceText)
		}
		out = append(out, ref)
	}
	return out
}

func validateFactDumpEvidenceIDs(rec factDumpRecord) error {
	if rec.Batch == nil {
		return nil
	}
	allowedEvidence := map[string]struct{}{}
	allowedMessage := map[string]struct{}{}
	aliases := map[string]string{}
	addAllowed := func(set map[string]struct{}, id string) {
		if id = strings.TrimSpace(id); id != "" {
			set[id] = struct{}{}
		}
	}
	addAlias := func(a, b string) {
		a = strings.TrimSpace(a)
		b = strings.TrimSpace(b)
		if a == "" && b == "" {
			return
		}
		rep := firstNonEmpty(a, b)
		if a != "" {
			aliases[a] = rep
		}
		if b != "" {
			aliases[b] = rep
		}
	}
	for _, id := range rec.Batch.EvidenceIDs {
		addAllowed(allowedEvidence, id)
		addAlias(id, "")
	}
	for _, id := range rec.Batch.SourceMessageIDs {
		addAllowed(allowedMessage, id)
		addAlias(id, "")
	}
	for _, turn := range rec.Batch.Turns {
		id := strings.TrimSpace(turn.ID)
		evidenceID := strings.TrimSpace(turn.EvidenceID)
		addAllowed(allowedMessage, id)
		addAllowed(allowedEvidence, evidenceID)
		addAllowed(allowedEvidence, id)
		addAlias(evidenceID, id)
	}
	strictEvidence := len(allowedEvidence) > 0
	strictMessage := len(allowedMessage) > 0
	for _, fact := range rec.Facts {
		for _, id := range fact.EvidenceIDs {
			id = strings.TrimSpace(id)
			if id != "" && strictEvidence {
				if _, ok := allowedEvidence[id]; !ok {
					return fmt.Errorf("facts dump evidence id %q is outside batch source ids", id)
				}
			}
		}
		for _, id := range fact.SourceMessageIDs {
			id = strings.TrimSpace(id)
			if id != "" && strictMessage {
				_, messageOK := allowedMessage[id]
				_, evidenceOK := allowedEvidence[id]
				if !messageOK && !evidenceOK {
					return fmt.Errorf("facts dump source message id %q is outside batch source ids", id)
				}
			}
		}
		for _, ref := range fact.EvidenceRefs {
			refID := strings.TrimSpace(ref.ID)
			msgID := strings.TrimSpace(ref.MessageID)
			if refID != "" && strictEvidence {
				if _, ok := allowedEvidence[refID]; !ok {
					return fmt.Errorf("facts dump evidence ref id %q is outside batch source ids", refID)
				}
			}
			if msgID != "" && refID == "" && strictMessage {
				if _, ok := allowedMessage[msgID]; !ok {
					return fmt.Errorf("facts dump evidence ref message_id %q is outside batch source ids", msgID)
				}
			}
			if refID != "" && msgID != "" {
				left, lok := aliases[refID]
				right, rok := aliases[msgID]
				if lok && rok && left != right {
					return fmt.Errorf("facts dump evidence ref mixes id %q with unrelated message_id %q", refID, msgID)
				}
			}
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func parseFactDumpDate(value string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02", time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported date format")
}
