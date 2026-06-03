package graphledger

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// BuildDelta constructs the canonical graph rows implied by the supplied write
// inputs. Save and Rebuild both use this package so graph ids and merge keys
// stay deterministic across online writes and offline repair.
func BuildDelta(scope domain.Scope, facts []domain.TemporalFact, closes []domain.ValidityClose, turns []domain.TurnContext, observedAt, now time.Time, requestID string) domain.MemoryGraphDelta {
	observations := make([]domain.Observation, 0, len(turns))
	observationByKey := make(map[string]int)
	addObservation := func(o domain.Observation) string {
		if o.ID == "" {
			return ""
		}
		if idx, ok := observationByKey[o.ID]; ok {
			merged, _, conflict := domain.MergeObservation(observations[idx], o)
			if conflict {
				return ""
			}
			observations[idx] = merged
		} else {
			observationByKey[o.ID] = len(observations)
			observations = append(observations, o.Clone())
		}
		return o.ID
	}

	for i, turn := range turns {
		obs := ObservationFromTurn(scope, turn, i, observedAt, now, requestID)
		addObservation(obs)
	}

	assertions := cloneFacts(facts)
	StampFactEvidenceRefs(scope, assertions, requestID)

	links := make([]domain.FactLink, 0)
	linkSeen := make(map[string]struct{})
	addLink := func(l domain.FactLink) {
		if l.ID == "" || l.MergeKey == "" {
			return
		}
		if _, ok := linkSeen[l.MergeKey]; ok {
			return
		}
		linkSeen[l.MergeKey] = struct{}{}
		links = append(links, l.Clone())
	}

	assertionsByObservation := make(map[string][]domain.TemporalFact)
	for _, fact := range assertions {
		observationIDsForFact := make([]string, 0, len(fact.EvidenceRefs))
		evidenceRefsForFact := normalizedEvidenceRefs(fact.EvidenceRefs)
		for i, ref := range fact.EvidenceRefs {
			obs := ObservationFromEvidenceRef(fact.Scope, ref, fact.ID, i, now, requestID)
			obsID := addObservation(obs)
			if obsID == "" {
				continue
			}
			observationIDsForFact = append(observationIDsForFact, obsID)
			assertionsByObservation[obsID] = append(assertionsByObservation[obsID], fact)
			refForObservationLink := ref
			refForObservationLink.ObservationID = obsID
			addLink(NewFactObservationLink(fact.Scope, domain.LinkDerivedFrom, fact.ID, obsID, []domain.EvidenceRef{refForObservationLink}, now))
			addLink(NewObservationFactLink(fact.Scope, domain.LinkSupports, obsID, fact.ID, []domain.EvidenceRef{refForObservationLink}, now))
			for _, span := range obs.Spans {
				if span.ID == "" {
					continue
				}
				refForLink := ref
				refForLink.ObservationID = obsID
				refForLink.SpanID = span.ID
				addLink(NewFactObservationSpanLink(fact.Scope, domain.LinkDerivedFrom, fact.ID, span.ID, []domain.EvidenceRef{refForLink}, now))
				addLink(NewObservationSpanFactLink(fact.Scope, domain.LinkSupports, span.ID, fact.ID, []domain.EvidenceRef{refForLink}, now))
			}
		}
		for _, priorID := range fact.Supersedes {
			addLink(NewAssertionAssertionLink(fact.Scope, domain.LinkSupersedes, fact.ID, priorID, observationIDsForFact, evidenceRefsForFact, now))
		}
	}
	for observationID, facts := range assertionsByObservation {
		addSameObservationLinks(addLink, observationID, facts, now)
	}
	for _, close := range closes {
		if close.CorrectedBy == "" || close.FactID == "" {
			continue
		}
		addLink(NewAssertionAssertionLink(close.Scope, domain.LinkSupersedes, close.CorrectedBy, close.FactID, nil, nil, now))
	}

	return domain.MemoryGraphDelta{
		Observations: observations,
		Assertions:   assertions,
		Links:        links,
		Closes:       append([]domain.ValidityClose(nil), closes...),
	}
}

// StampFactEvidenceRefs fills canonical Observation/Span references on fact
// evidence in-place. It is intentionally separate from BuildDelta so the
// assertion rows stored by TemporalStore carry the same canonical refs.
func StampFactEvidenceRefs(scope domain.Scope, facts []domain.TemporalFact, requestID string) {
	for fi := range facts {
		factScope := facts[fi].Scope
		if factScope.RuntimeID == "" && factScope.UserID == "" {
			factScope = scope
		}
		for ri := range facts[fi].EvidenceRefs {
			facts[fi].EvidenceRefs[ri] = CanonicalEvidenceRef(factScope, facts[fi].EvidenceRefs[ri], facts[fi].ID, ri, requestID)
		}
	}
}

func CanonicalEvidenceRef(scope domain.Scope, ref domain.EvidenceRef, factID string, index int, requestID string) domain.EvidenceRef {
	out := ref
	if out.RequestID == "" {
		out.RequestID = strings.TrimSpace(requestID)
	}
	sourceID := observationSourceID(out)
	if out.ObservationID == "" {
		out.ObservationID = stableObservationIDForSource(scope, out.RequestID, out.SessionID, sourceID, fmt.Sprintf("evidence:%s:%d:%s", factID, index, out.Text))
	}
	if out.SpanID == "" {
		out.SpanID = StableObservationSpanID(out.ObservationID, sourceID, domain.ObservationSpanKindQuote, 0, len(out.Text), out.Text)
	}
	return out
}

func ObservationFromTurn(scope domain.Scope, turn domain.TurnContext, index int, observedAt, now time.Time, requestID string) domain.Observation {
	ts := turn.Time
	if ts.IsZero() {
		ts = observedAt
	}
	if ts.IsZero() {
		ts = now
	}
	sourceID := turn.EvidenceID
	if sourceID == "" {
		sourceID = turn.ID
	}
	id := stableObservationIDForSource(scope, requestID, turn.SessionID, sourceID, fmt.Sprintf("turn:%d:%s", index, turn.Text))
	span := ObservationSpanFromText(id, sourceID, domain.ObservationSpanKindText, turn.Text, 0, len(turn.Text))
	return domain.Observation{
		ID:         id,
		Scope:      scope,
		Kind:       domain.ObservationKindTurn,
		SourceID:   sourceID,
		SessionID:  turn.SessionID,
		MessageID:  turn.ID,
		Role:       turn.Role,
		Speaker:    turn.Speaker,
		Text:       turn.Text,
		Spans:      []domain.ObservationSpan{span},
		ObservedAt: ts,
		ReceivedAt: now,
	}
}

func ObservationFromEvidenceRef(scope domain.Scope, ref domain.EvidenceRef, factID string, index int, now time.Time, requestID string) domain.Observation {
	ts := ref.Timestamp
	if ts.IsZero() {
		ts = now
	}
	canonicalRef := CanonicalEvidenceRef(scope, ref, factID, index, requestID)
	sourceID := observationSourceID(canonicalRef)
	id := canonicalRef.ObservationID
	span := ObservationSpanFromText(id, sourceID, domain.ObservationSpanKindQuote, canonicalRef.Text, 0, len(canonicalRef.Text))
	span.ID = canonicalRef.SpanID
	return domain.Observation{
		ID:         id,
		Scope:      scope,
		Kind:       domain.ObservationKindEvidence,
		SourceID:   sourceID,
		SessionID:  canonicalRef.SessionID,
		MessageID:  ref.MessageID,
		Role:       ref.Role,
		Text:       ref.Text,
		Spans:      []domain.ObservationSpan{span},
		ObservedAt: ts,
		ReceivedAt: now,
	}
}

func ObservationSpanFromText(observationID, sourceID string, kind domain.ObservationSpanKind, text string, start, end int) domain.ObservationSpan {
	return domain.ObservationSpan{
		ID:            StableObservationSpanID(observationID, sourceID, kind, start, end, text),
		ObservationID: observationID,
		SourceID:      sourceID,
		Kind:          kind,
		Text:          text,
		Start:         start,
		End:           end,
	}
}

func NewFactObservationSpanLink(scope domain.Scope, typ domain.FactLinkType, factID, spanID string, evidenceRefs []domain.EvidenceRef, now time.Time) domain.FactLink {
	from := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: factID}
	to := domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: spanID}
	return NewLink(scope, typ, from, to, evidenceObservationIDs(evidenceRefs), evidenceRefs, now)
}

func NewObservationSpanFactLink(scope domain.Scope, typ domain.FactLinkType, spanID, factID string, evidenceRefs []domain.EvidenceRef, now time.Time) domain.FactLink {
	from := domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: spanID}
	to := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: factID}
	return NewLink(scope, typ, from, to, evidenceObservationIDs(evidenceRefs), evidenceRefs, now)
}

func NewFactObservationLink(scope domain.Scope, typ domain.FactLinkType, factID, observationID string, evidenceRefs []domain.EvidenceRef, now time.Time) domain.FactLink {
	from := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: factID}
	to := domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: observationID}
	return NewLink(scope, typ, from, to, evidenceObservationIDs(evidenceRefs), evidenceRefs, now)
}

func NewObservationFactLink(scope domain.Scope, typ domain.FactLinkType, observationID, factID string, evidenceRefs []domain.EvidenceRef, now time.Time) domain.FactLink {
	from := domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: observationID}
	to := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: factID}
	return NewLink(scope, typ, from, to, evidenceObservationIDs(evidenceRefs), evidenceRefs, now)
}

func NewAssertionAssertionLink(scope domain.Scope, typ domain.FactLinkType, fromFactID, toFactID string, evidenceObservationIDs []string, evidenceRefs []domain.EvidenceRef, now time.Time) domain.FactLink {
	from := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: fromFactID}
	to := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: toFactID}
	return NewLink(scope, typ, from, to, evidenceObservationIDs, evidenceRefs, now)
}

func addSameObservationLinks(addLink func(domain.FactLink), observationID string, facts []domain.TemporalFact, now time.Time) {
	if len(facts) < 2 {
		return
	}
	sort.SliceStable(facts, func(i, j int) bool { return facts[i].ID < facts[j].ID })
	for i := 0; i < len(facts); i++ {
		for j := i + 1; j < len(facts); j++ {
			a, b := facts[i], facts[j]
			if a.ID == "" || b.ID == "" {
				continue
			}
			evidenceIDs := []string{observationID}
			refs := sharedEvidenceRefs(a.EvidenceRefs, b.EvidenceRefs, observationID)
			addLink(NewAssertionAssertionLink(a.Scope, domain.LinkSameObservation, a.ID, b.ID, evidenceIDs, refs, now))
			addLink(NewAssertionAssertionLink(a.Scope, domain.LinkSameEventAs, a.ID, b.ID, evidenceIDs, refs, now))
		}
	}
}

func sharedEvidenceRefs(a, b []domain.EvidenceRef, observationID string) []domain.EvidenceRef {
	seen := map[string]domain.EvidenceRef{}
	for _, ref := range append(normalizedEvidenceRefs(a), normalizedEvidenceRefs(b)...) {
		if observationID != "" && ref.ObservationID != "" && ref.ObservationID != observationID {
			continue
		}
		key := ref.ID
		if key == "" {
			key = strings.Join([]string{ref.ObservationID, ref.SpanID, ref.MessageID, ref.Text}, "\x00")
		}
		if key == "" {
			continue
		}
		seen[key] = ref
	}
	out := make([]domain.EvidenceRef, 0, len(seen))
	for _, ref := range seen {
		out = append(out, ref)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func NewLink(scope domain.Scope, typ domain.FactLinkType, from, to domain.GraphNodeRef, evidenceObservationIDs []string, evidenceRefs []domain.EvidenceRef, now time.Time) domain.FactLink {
	evidenceRefs = normalizedEvidenceRefs(evidenceRefs)
	mergeKey := StableLinkMergeKey(scope, typ, from, to, evidenceRefs)
	return domain.FactLink{
		ID:                     "lnk_" + shortHash(mergeKey),
		Scope:                  scope,
		Type:                   typ,
		From:                   from,
		To:                     to,
		MergeKey:               mergeKey,
		Confidence:             1,
		CreatedAt:              now,
		EvidenceObservationIDs: uniqueSortedStrings(evidenceObservationIDs),
		EvidenceRefs:           evidenceRefs,
	}
}

func StableObservationID(scope domain.Scope, kind, sourceID, fallback, text string) string {
	key := strings.Join([]string{scope.RuntimeID, scope.UserID, kind, sourceID, fallback, text}, "\x00")
	return "obs_" + shortHash(key)
}

func StableSourceObservationID(scope domain.Scope, requestID, sessionID, sourceID string) string {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return ""
	}
	return StableObservationID(scope, "source", sourceID, strings.Join([]string{requestID, sessionID}, "\x00"), "")
}

func StableObservationSpanID(observationID, sourceID string, kind domain.ObservationSpanKind, start, end int, text string) string {
	key := strings.Join([]string{observationID, sourceID, string(kind), fmt.Sprintf("%d:%d", start, end), text}, "\x00")
	return "osp_" + shortHash(key)
}

func StableLinkMergeKey(scope domain.Scope, typ domain.FactLinkType, from, to domain.GraphNodeRef, evidenceRefs []domain.EvidenceRef) string {
	parts := []string{
		scope.RuntimeID,
		scope.UserID,
		string(typ),
		string(from.Kind),
		from.ID,
		string(to.Kind),
		to.ID,
	}
	parts = append(parts, evidenceRefKeys(evidenceRefs)...)
	return strings.Join(parts, "\x00")
}

func stableObservationIDForSource(scope domain.Scope, requestID, sessionID, sourceID, fallback string) string {
	if id := StableSourceObservationID(scope, strings.TrimSpace(requestID), strings.TrimSpace(sessionID), sourceID); id != "" {
		return id
	}
	return StableObservationID(scope, "evidence", strings.TrimSpace(sourceID), strings.Join([]string{strings.TrimSpace(requestID), strings.TrimSpace(sessionID), fallback}, "\x00"), "")
}

func observationSourceID(ref domain.EvidenceRef) string {
	if ref.ID != "" {
		return strings.TrimSpace(ref.ID)
	}
	return strings.TrimSpace(ref.MessageID)
}

func normalizedEvidenceRefs(refs []domain.EvidenceRef) []domain.EvidenceRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]domain.EvidenceRef, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		key := evidenceRefKey(ref)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ref)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return evidenceRefKey(out[i]) < evidenceRefKey(out[j])
	})
	return out
}

func evidenceRefKeys(refs []domain.EvidenceRef) []string {
	if len(refs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		if key := evidenceRefKey(ref); key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func evidenceRefKey(ref domain.EvidenceRef) string {
	parts := []string{
		strings.TrimSpace(ref.RequestID),
		strings.TrimSpace(ref.SessionID),
		strings.TrimSpace(ref.ObservationID),
		strings.TrimSpace(ref.SpanID),
		strings.TrimSpace(ref.ID),
		strings.TrimSpace(ref.MessageID),
	}
	for _, part := range parts {
		if part != "" {
			return strings.Join(parts, "\x1f")
		}
	}
	return ""
}

func evidenceObservationIDs(refs []domain.EvidenceRef) []string {
	if len(refs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.ObservationID != "" {
			ids = append(ids, ref.ObservationID)
		}
	}
	return uniqueSortedStrings(ids)
}

func uniqueSortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func shortHash(in string) string {
	sum := sha256.Sum256([]byte(in))
	return hex.EncodeToString(sum[:])[:24]
}

func cloneFacts(facts []domain.TemporalFact) []domain.TemporalFact {
	if len(facts) == 0 {
		return nil
	}
	out := make([]domain.TemporalFact, 0, len(facts))
	for _, f := range facts {
		out = append(out, f.Clone())
	}
	return out
}
