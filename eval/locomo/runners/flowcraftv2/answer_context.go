package flowcraftv2

import (
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/recall"
)

func structuredAnswerContext(question runners.AnswerQuestion, hits []recall.Hit) runners.AnswerContext {
	return runners.AnswerContext{
		Body:           renderStructuredAnswerBody(question, hits),
		Format:         "flowcraftv2_structured_facts",
		PromptTemplate: structuredFactsAnswerPrompt,
	}
}

const structuredFactsAnswerPrompt = `You are answering a question using only the structured memory facts below.

Guidelines:
- Ground the answer strictly in the structured facts and evidence quotes. Do not invent facts that are not supported.
- Each memory is listed in retrieval/rerank order as [#1], [#2], etc. Prefer lower-numbered memories when evidence conflicts, but combine compatible facts for list and bridge questions.
- Treat event_time as the event date. event_time_source and event_time_text explain how that date was derived.
- Treat observed_at and evidence source_time as evidence timestamps, not event dates by themselves. Use source_time only as the anchor for relative wording in the same quote.
- If content or event_time_text uses a date qualifier ("around", "roughly", "the week before X", "a few years ago", "last summer", "two weekends ago"), preserve that qualifier when it is the best-supported answer rather than fabricating precision.
- Match the form of the question. If asked WHEN, give a date or duration; HOW MANY, a number; YES/NO, lead with yes/no.
- For list or set questions, extract literal named items from all relevant facts and return a compact comma-separated list.
- For bridge questions, resolve placeholders by combining relevant facts before answering.
- Prefer exact spans from content, object, location, entities, participants, and evidence quotes over broad paraphrases.
- Treat content as the canonical extracted fact. Evidence quotes are grounding snippets and may be partial; do not ignore a directly relevant content field just because the quote omits surrounding context.
- For yes/no or likely/would questions, make the best-supported yes/no inference from the facts. If the facts support an alternative or contradict the proposition, answer "no" with that reason instead of "I don't know".
- When an ASKED_AT line is present, treat that timestamp as the "now" for the question.
- When EVIDENCE_PACKAGE is present, use primary_ranks as the main evidence and supporting_ranks only to complete list/set, bridge, or temporal details. Do not let supporting evidence override a stronger conflicting primary fact.
- Answer in 1-2 sentences. Avoid hedging when the facts are unambiguous. Reply "I don't know" only when the structured facts are genuinely silent on the topic.

%s

Answer:`

func renderStructuredAnswerBody(question runners.AnswerQuestion, hits []recall.Hit) string {
	var b strings.Builder
	if asked := strings.TrimSpace(question.AskedAt); asked != "" {
		b.WriteString("ASKED_AT: ")
		b.WriteString(asked)
		b.WriteString("\n\n")
	}
	b.WriteString("QUESTION: ")
	b.WriteString(strings.TrimSpace(question.Query))
	b.WriteString("\n\n")
	renderEvidencePackage(&b, question, hits)
	b.WriteString("MEMORIES (STRUCTURED_FACTS):\n")
	if len(hits) == 0 {
		b.WriteString("(none)\n")
		return b.String()
	}
	for i, hit := range hits {
		renderStructuredHit(&b, i+1, hit)
	}
	return b.String()
}

func renderEvidencePackage(b *strings.Builder, question runners.AnswerQuestion, hits []recall.Hit) {
	pkg := buildEvidencePackage(question.Query, hits)
	if len(pkg.types) == 0 {
		return
	}
	b.WriteString("EVIDENCE_PACKAGE:\n")
	writeOrderedListKV(b, 1, "types", pkg.types)
	writeOrderedListKV(b, 1, "primary_ranks", pkg.primaryRanks)
	writeOrderedListKV(b, 1, "supporting_ranks", pkg.supportingRanks)
	b.WriteString("\n")
}

type evidencePackage struct {
	types           []string
	primaryRanks    []string
	supportingRanks []string
}

func buildEvidencePackage(query string, hits []recall.Hit) evidencePackage {
	types := evidencePackageLabels(query)
	if len(types) == 0 || len(hits) == 0 {
		return evidencePackage{types: types}
	}
	primaryCount := min(5, len(hits))
	pkg := evidencePackage{types: types}
	for i := 0; i < primaryCount; i++ {
		pkg.primaryRanks = append(pkg.primaryRanks, rankLabel(i))
	}
	supporting := supportingEvidenceRanks(types, hits, primaryCount)
	for _, rank := range supporting {
		pkg.supportingRanks = append(pkg.supportingRanks, rankLabel(rank))
	}
	return pkg
}

func evidencePackageLabels(query string) []string {
	text := strings.ToLower(query)
	labels := []string{}
	if strings.Contains(text, "how many") ||
		strings.Contains(text, "what ") && (strings.Contains(text, " has ") || strings.Contains(text, " have ")) {
		labels = append(labels, "set_completion")
	}
	if strings.Contains(text, " that ") ||
		strings.Contains(text, " which ") ||
		strings.Contains(text, " who ") ||
		strings.Contains(text, " her ") ||
		strings.Contains(text, " his ") ||
		strings.Contains(text, " their ") {
		labels = append(labels, "bridge_chain")
	}
	if strings.Contains(text, "when") || strings.Contains(text, "before") || strings.Contains(text, "after") || strings.Contains(text, "how long") {
		labels = append(labels, "temporal_anchor")
	}
	if len(labels) == 0 {
		return nil
	}
	return append([]string{"direct"}, nonEmptyStrings(labels)...)
}

func supportingEvidenceRanks(types []string, hits []recall.Hit, primaryCount int) []int {
	if primaryCount <= 0 || primaryCount >= len(hits) {
		return nil
	}
	setIntent := containsString(types, "set_completion")
	bridgeIntent := containsString(types, "bridge_chain")
	temporalIntent := containsString(types, "temporal_anchor")
	primary := hits[:primaryCount]
	out := make([]int, 0, 6)
	for i := primaryCount; i < len(hits) && len(out) < 6; i++ {
		hit := hits[i]
		switch {
		case setIntent && packageSetSibling(hit, primary):
			out = append(out, i)
		case bridgeIntent && packageBridgeSibling(hit, primary):
			out = append(out, i)
		case temporalIntent && packageTemporalAnchor(hit):
			out = append(out, i)
		}
	}
	return out
}

func packageSetSibling(hit recall.Hit, primary []recall.Hit) bool {
	for _, existing := range primary {
		if sameSubjectPredicatePublic(hit.Fact, existing.Fact) {
			return true
		}
	}
	return false
}

func packageBridgeSibling(hit recall.Hit, primary []recall.Hit) bool {
	group := packageEvidenceGroup(hit)
	for _, existing := range primary {
		if group != "" && group == packageEvidenceGroup(existing) {
			return true
		}
		if packageFactSibling(hit.Fact, existing.Fact) {
			return true
		}
	}
	return false
}

func packageTemporalAnchor(hit recall.Hit) bool {
	return hit.Fact.ValidFrom != nil && !hit.Fact.ValidFrom.IsZero()
}

func sameSubjectPredicatePublic(a, b recall.TemporalFact) bool {
	return strings.TrimSpace(a.Subject) != "" &&
		strings.TrimSpace(b.Subject) != "" &&
		strings.EqualFold(a.Subject, b.Subject) &&
		strings.TrimSpace(a.Predicate) != "" &&
		strings.TrimSpace(b.Predicate) != "" &&
		strings.EqualFold(a.Predicate, b.Predicate)
}

func packageFactSibling(a, b recall.TemporalFact) bool {
	if a.ID != "" && a.ID == b.ID {
		return false
	}
	if sameSubjectPredicatePublic(a, b) {
		return true
	}
	if a.Subject != "" && b.Subject != "" && strings.EqualFold(a.Subject, b.Subject) {
		return a.Kind == b.Kind
	}
	if a.Predicate != "" && b.Predicate != "" && strings.EqualFold(a.Predicate, b.Predicate) {
		return a.Kind == b.Kind
	}
	return false
}

func packageEvidenceGroup(hit recall.Hit) string {
	evidence := hit.Evidence
	if len(evidence) == 0 {
		evidence = hit.Fact.EvidenceRefs
	}
	for _, ref := range evidence {
		for _, raw := range []string{ref.ID, ref.MessageID} {
			if group := packageEvidenceIDGroup(raw); group != "" {
				return group
			}
		}
	}
	return ""
}

func packageEvidenceIDGroup(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ';' || r == ',' || r == ' '
	})
	if len(parts) == 0 {
		return ""
	}
	raw = parts[0]
	idx := strings.LastIndex(raw, ":")
	if idx <= 0 || idx == len(raw)-1 {
		return ""
	}
	for _, r := range raw[idx+1:] {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return raw[:idx]
}

func rankLabel(idx int) string {
	return fmt.Sprintf("#%d", idx+1)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func renderStructuredHit(b *strings.Builder, rank int, hit recall.Hit) {
	f := hit.Fact
	fmt.Fprintf(b, "- [#%d]\n", rank)
	writeKV(b, 1, "fact_id", f.ID)
	writeKV(b, 1, "kind", string(f.Kind))
	writeKV(b, 1, "score", fmt.Sprintf("%.6f", hit.Score))
	writeListKV(b, 1, "sources", answerVisibleSources(hit.Sources))
	writeKV(b, 1, "content", f.Content)
	writeKV(b, 1, "subject", f.Subject)
	writeKV(b, 1, "predicate", f.Predicate)
	writeKV(b, 1, "object", f.Object)
	writeListKV(b, 1, "entities", f.Entities)
	writeListKV(b, 1, "participants", f.Participants)
	writeKV(b, 1, "location", f.Location)
	if f.ValidFrom != nil && !f.ValidFrom.IsZero() && rendersEventTime(f.Metadata) {
		writeKV(b, 1, "event_time", f.ValidFrom.Format("2006-01-02"))
		writeKV(b, 1, "event_time_source", metadataString(f.Metadata, validFromSourceMetadataKey))
		writeKV(b, 1, "event_time_text", metadataString(f.Metadata, "valid_from_text"))
	}
	if !f.ObservedAt.IsZero() {
		writeKV(b, 1, "observed_at", f.ObservedAt.Format("2006-01-02 15:04"))
	}
	if f.EvidenceText != "" {
		writeKV(b, 1, "fact_evidence_text", f.EvidenceText)
	}
	evidence := hit.Evidence
	if len(evidence) == 0 {
		evidence = f.EvidenceRefs
	}
	writeListKV(b, 1, "evidence_ids", evidenceIDs(evidence))
	if len(evidence) > 0 {
		writeIndent(b, 1)
		b.WriteString("evidence:\n")
		for _, ref := range evidence {
			renderStructuredEvidence(b, ref)
		}
	}
}

func answerVisibleSources(sources []string) []string {
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		if source == "neighbor_candidate" {
			continue
		}
		out = append(out, source)
	}
	return out
}

func renderStructuredEvidence(b *strings.Builder, ref recall.EvidenceRef) {
	writeIndent(b, 2)
	b.WriteString("-")
	if ref.ID != "" {
		b.WriteString(" id: ")
		b.WriteString(quoteScalar(ref.ID))
	}
	b.WriteString("\n")
	writeKV(b, 3, "message_id", ref.MessageID)
	writeKV(b, 3, "role", ref.Role)
	if !ref.Timestamp.IsZero() {
		writeKV(b, 3, "source_time", evidenceSourceTimeLabel(ref))
	}
	writeKV(b, 3, "quote", ref.Text)
}

func writeKV(b *strings.Builder, indent int, key, value string) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	if value == "" {
		return
	}
	writeIndent(b, indent)
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(quoteScalar(value))
	b.WriteString("\n")
}

func writeListKV(b *strings.Builder, indent int, key string, values []string) {
	values = nonEmptyStrings(values)
	if len(values) == 0 {
		return
	}
	writeIndent(b, indent)
	b.WriteString(key)
	b.WriteString(": ")
	for i, value := range values {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quoteScalar(value))
	}
	b.WriteString("\n")
}

func writeOrderedListKV(b *strings.Builder, indent int, key string, values []string) {
	values = nonEmptyStringsPreserveOrder(values)
	if len(values) == 0 {
		return
	}
	writeIndent(b, indent)
	b.WriteString(key)
	b.WriteString(": ")
	for i, value := range values {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quoteScalar(value))
	}
	b.WriteString("\n")
}

func writeIndent(b *strings.Builder, indent int) {
	for range indent {
		b.WriteString("  ")
	}
}

func quoteScalar(value string) string {
	return fmt.Sprintf("%q", strings.TrimSpace(value))
}

func metadataString(meta map[string]any, key string) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[key]
	if !ok {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func nonEmptyStringsPreserveOrder(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func evidenceIDs(evidence []recall.EvidenceRef) []string {
	out := make([]string, 0, len(evidence))
	for _, ref := range evidence {
		if ref.ID != "" {
			out = append(out, ref.ID)
		}
	}
	return out
}
