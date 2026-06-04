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

	links := make([]domain.FactLink, 0)
	linkSeen := make(map[string]struct{})
	supersedesSeen := make(map[string]struct{})
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
		evidenceRefsForFact := canonicalEvidenceRefs(fact.EvidenceRefs)
		for _, ref := range evidenceRefsForFact {
			if ref.RequestID == "" {
				ref.RequestID = strings.TrimSpace(requestID)
			}
			observationIDsForFact = append(observationIDsForFact, ref.ObservationID)
			assertionsByObservation[ref.ObservationID] = append(assertionsByObservation[ref.ObservationID], fact)
			addLink(NewFactObservationLink(fact.Scope, domain.LinkDerivedFrom, fact.ID, ref.ObservationID, []domain.EvidenceRef{ref}, now))
			addLink(NewObservationFactLink(fact.Scope, domain.LinkSupports, ref.ObservationID, fact.ID, []domain.EvidenceRef{ref}, now))
			addLink(NewFactObservationSpanLink(fact.Scope, domain.LinkDerivedFrom, fact.ID, ref.SpanID, []domain.EvidenceRef{ref}, now))
			addLink(NewObservationSpanFactLink(fact.Scope, domain.LinkSupports, ref.SpanID, fact.ID, []domain.EvidenceRef{ref}, now))
		}
		for _, priorID := range fact.Supersedes {
			supersedesSeen[assertionPairKey(fact.ID, priorID)] = struct{}{}
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
		if _, ok := supersedesSeen[assertionPairKey(close.CorrectedBy, close.FactID)]; ok {
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

func IsGeneratedQuoteEvidenceRef(ref domain.EvidenceRef) bool {
	if ref.ObservationID == "" || ref.SpanID == "" || ref.Text == "" {
		return false
	}
	return ref.SpanID == StableObservationSpanID(ref.ObservationID, observationSourceID(ref), domain.ObservationSpanKindQuote, 0, len(ref.Text), ref.Text)
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
	spans := SegmentObservationText(id, sourceID, turn.Text)
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
		Spans:      spans,
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

// SegmentObservationText preserves replayable source spans for Save grounding.
// It keeps a full turn span and then adds stable paragraph/list/table/sentence
// spans without lower-casing or tokenizing the source text.
func SegmentObservationText(observationID, sourceID, text string) []domain.ObservationSpan {
	if text == "" {
		return nil
	}
	var spans []domain.ObservationSpan
	add := func(kind domain.ObservationSpanKind, start, end int) {
		if start < 0 || end > len(text) || start >= end {
			return
		}
		spanText := text[start:end]
		if strings.TrimSpace(spanText) == "" {
			return
		}
		spans = append(spans, ObservationSpanFromText(observationID, sourceID, kind, spanText, start, end))
	}
	add(domain.ObservationSpanKindTurn, 0, len(text))
	for _, block := range lineBlocks(text) {
		kind := domain.ObservationSpanKindParagraph
		trimmed := strings.TrimSpace(text[block.start:block.end])
		if isListItem(trimmed) {
			kind = domain.ObservationSpanKindListItem
		} else if isTableRow(trimmed) {
			kind = domain.ObservationSpanKindTableRow
		}
		add(kind, block.start, block.end)
		if kind == domain.ObservationSpanKindParagraph {
			for _, sentence := range sentenceSpans(text, block.start, block.end) {
				add(domain.ObservationSpanKindSentence, sentence.start, sentence.end)
			}
		}
	}
	return dedupeObservationSpans(spans)
}

type byteSpan struct {
	start int
	end   int
}

func lineBlocks(text string) []byteSpan {
	var out []byteSpan
	start := 0
	lineStart := 0
	blankRun := false
	flush := func(end int) {
		for start < end && (text[start] == '\n' || text[start] == '\r') {
			start++
		}
		for end > start && (text[end-1] == '\n' || text[end-1] == '\r') {
			end--
		}
		if start < end {
			out = append(out, byteSpan{start: start, end: end})
		}
	}
	for i, r := range text {
		if r != '\n' {
			continue
		}
		line := strings.TrimSpace(text[lineStart:i])
		if line == "" {
			flush(lineStart)
			start = i + 1
			blankRun = true
		} else if blankRun || isListItem(line) || isTableRow(line) {
			if lineStart > start {
				flush(lineStart)
				start = lineStart
			}
			flush(i)
			start = i + 1
			blankRun = false
		} else {
			blankRun = false
		}
		lineStart = i + 1
	}
	flush(len(text))
	return out
}

func sentenceSpans(text string, start, end int) []byteSpan {
	var out []byteSpan
	segStart := start
	for i, r := range text[start:end] {
		abs := start + i
		switch r {
		case '.', '?', '!', '。', '？', '！', '；':
			segEnd := abs + len(string(r))
			if segEnd > segStart {
				out = append(out, byteSpan{start: trimLeftByte(text, segStart, segEnd), end: trimRightByte(text, segStart, segEnd)})
			}
			segStart = segEnd
		}
	}
	if segStart < end {
		out = append(out, byteSpan{start: trimLeftByte(text, segStart, end), end: trimRightByte(text, segStart, end)})
	}
	return out
}

func trimLeftByte(text string, start, end int) int {
	for start < end && (text[start] == ' ' || text[start] == '\t' || text[start] == '\n' || text[start] == '\r') {
		start++
	}
	return start
}

func trimRightByte(text string, start, end int) int {
	for end > start && (text[end-1] == ' ' || text[end-1] == '\t' || text[end-1] == '\n' || text[end-1] == '\r') {
		end--
	}
	return end
}

func isListItem(line string) bool {
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		return true
	}
	if len(line) < 3 {
		return false
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(line) && (line[i] == '.' || line[i] == ')') && line[i+1] == ' '
}

func isTableRow(line string) bool {
	if strings.Count(line, "|") >= 2 {
		return true
	}
	return strings.Count(line, "\t") >= 1
}

func dedupeObservationSpans(spans []domain.ObservationSpan) []domain.ObservationSpan {
	seen := map[string]struct{}{}
	out := make([]domain.ObservationSpan, 0, len(spans))
	for _, span := range spans {
		key := string(span.Kind) + "\x00" + fmt.Sprintf("%d:%d", span.Start, span.End) + "\x00" + span.Text
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, span)
	}
	return out
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
	if typ == domain.LinkSameObservation && toFactID < fromFactID {
		fromFactID, toFactID = toFactID, fromFactID
	}
	from := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: fromFactID}
	to := domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: toFactID}
	return NewLink(scope, typ, from, to, evidenceObservationIDs, evidenceRefs, now)
}

func assertionPairKey(fromFactID, toFactID string) string {
	return strings.TrimSpace(fromFactID) + "\x00" + strings.TrimSpace(toFactID)
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
	return StableObservationID(scope, "turn", strings.TrimSpace(sourceID), strings.Join([]string{strings.TrimSpace(requestID), strings.TrimSpace(sessionID), fallback}, "\x00"), "")
}

func observationSourceID(ref domain.EvidenceRef) string {
	if ref.ID != "" {
		return strings.TrimSpace(ref.ID)
	}
	return strings.TrimSpace(ref.MessageID)
}

func canonicalEvidenceRefs(refs []domain.EvidenceRef) []domain.EvidenceRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]domain.EvidenceRef, 0, len(refs))
	for _, ref := range refs {
		if ref.ObservationID == "" || ref.SpanID == "" || IsGeneratedQuoteEvidenceRef(ref) {
			continue
		}
		out = append(out, ref)
	}
	return normalizedEvidenceRefs(out)
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
