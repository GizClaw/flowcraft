package domain

import "strings"

// MergeObservation merges additional spans into an existing Observation with
// the same canonical ID. It rejects only source/base conflicts; repeating the
// same source object with more precise spans is valid ledger growth.
func MergeObservation(existing, incoming Observation) (Observation, bool, bool) {
	if existing.ID == "" || incoming.ID == "" || existing.ID != incoming.ID {
		return existing, false, true
	}
	if existing.Scope.RuntimeID != incoming.Scope.RuntimeID || existing.Scope.UserID != incoming.Scope.UserID {
		return existing, false, true
	}
	if !compatibleObservationBase(existing, incoming) {
		return existing, false, true
	}

	merged := existing.Clone()
	changed := false
	changed = fillObservationBase(&merged, incoming) || changed
	for _, span := range incoming.Spans {
		if span.ID == "" {
			continue
		}
		var found bool
		for i := range merged.Spans {
			if merged.Spans[i].ID != span.ID {
				continue
			}
			found = true
			mergedSpan, spanChanged, conflict := MergeObservationSpan(merged.Spans[i], span)
			if conflict {
				return existing, false, true
			}
			if spanChanged {
				merged.Spans[i] = mergedSpan
				changed = true
			}
			break
		}
		if !found {
			merged.Spans = append(merged.Spans, span.Clone())
			changed = true
		}
	}
	merged.Metadata, changed = mergeMetadata(merged.Metadata, incoming.Metadata, changed)
	return merged, changed, false
}

// MergeObservationSpan merges metadata for identical span IDs and validates
// that the address still refers to compatible text.
func MergeObservationSpan(existing, incoming ObservationSpan) (ObservationSpan, bool, bool) {
	if existing.ID == "" || incoming.ID == "" || existing.ID != incoming.ID {
		return existing, false, true
	}
	if existing.ObservationID != "" && incoming.ObservationID != "" && existing.ObservationID != incoming.ObservationID {
		return existing, false, true
	}
	if existing.Text != "" && incoming.Text != "" && !compatibleText(existing.Text, incoming.Text) {
		return existing, false, true
	}
	merged := existing.Clone()
	changed := false
	if merged.ObservationID == "" && incoming.ObservationID != "" {
		merged.ObservationID = incoming.ObservationID
		changed = true
	}
	if merged.SourceID == "" && incoming.SourceID != "" {
		merged.SourceID = incoming.SourceID
		changed = true
	}
	if merged.Kind == "" && incoming.Kind != "" {
		merged.Kind = incoming.Kind
		changed = true
	}
	if merged.Text == "" || (incoming.Text != "" && len(incoming.Text) > len(merged.Text) && compatibleText(merged.Text, incoming.Text)) {
		merged.Text = incoming.Text
		changed = true
	}
	if merged.Start == 0 && incoming.Start != 0 {
		merged.Start = incoming.Start
		changed = true
	}
	if merged.End == 0 && incoming.End != 0 {
		merged.End = incoming.End
		changed = true
	}
	merged.Metadata, changed = mergeMetadata(merged.Metadata, incoming.Metadata, changed)
	return merged, changed, false
}

func compatibleObservationBase(a, b Observation) bool {
	if a.SourceID != "" && b.SourceID != "" && a.SourceID != b.SourceID {
		return false
	}
	if a.SessionID != "" && b.SessionID != "" && a.SessionID != b.SessionID {
		return false
	}
	if a.MessageID != "" && b.MessageID != "" && a.MessageID != b.MessageID {
		return false
	}
	if a.Kind != "" && b.Kind != "" && a.Kind != b.Kind {
		if !(a.Kind == ObservationKindTurn && b.Kind == ObservationKindEvidence) &&
			!(a.Kind == ObservationKindEvidence && b.Kind == ObservationKindTurn) {
			return false
		}
	}
	if a.Text != "" && b.Text != "" && !compatibleText(a.Text, b.Text) && a.SourceID != b.SourceID {
		return false
	}
	return true
}

func fillObservationBase(merged *Observation, incoming Observation) bool {
	changed := false
	if merged.Kind == ObservationKindEvidence && incoming.Kind == ObservationKindTurn {
		merged.Kind = incoming.Kind
		changed = true
	}
	if merged.SourceID == "" && incoming.SourceID != "" {
		merged.SourceID = incoming.SourceID
		changed = true
	}
	if merged.SessionID == "" && incoming.SessionID != "" {
		merged.SessionID = incoming.SessionID
		changed = true
	}
	if merged.MessageID == "" && incoming.MessageID != "" {
		merged.MessageID = incoming.MessageID
		changed = true
	}
	if merged.Role == "" && incoming.Role != "" {
		merged.Role = incoming.Role
		changed = true
	}
	if merged.Speaker == "" && incoming.Speaker != "" {
		merged.Speaker = incoming.Speaker
		changed = true
	}
	if merged.Text == "" || (incoming.Text != "" && len(incoming.Text) > len(merged.Text) && compatibleText(merged.Text, incoming.Text)) {
		merged.Text = incoming.Text
		changed = true
	}
	if merged.ObservedAt.IsZero() || (!incoming.ObservedAt.IsZero() && incoming.ObservedAt.Before(merged.ObservedAt)) {
		merged.ObservedAt = incoming.ObservedAt
		changed = true
	}
	if merged.ReceivedAt.IsZero() || (!incoming.ReceivedAt.IsZero() && incoming.ReceivedAt.Before(merged.ReceivedAt)) {
		merged.ReceivedAt = incoming.ReceivedAt
		changed = true
	}
	return changed
}

func mergeMetadata(existing, incoming map[string]any, changed bool) (map[string]any, bool) {
	if len(incoming) == 0 {
		return existing, changed
	}
	if existing == nil {
		existing = map[string]any{}
	}
	for k, v := range incoming {
		if _, ok := existing[k]; ok {
			continue
		}
		existing[k] = v
		changed = true
	}
	return existing, changed
}

func compatibleText(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" || a == b {
		return true
	}
	aa := strings.ToLower(strings.Join(strings.Fields(a), " "))
	bb := strings.ToLower(strings.Join(strings.Fields(b), " "))
	return strings.Contains(aa, bb) || strings.Contains(bb, aa)
}
