package flowcraftv2

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/text/phrase"
)

func structuredAnswerContext(question runners.AnswerQuestion, hits []recall.Hit) runners.AnswerContext {
	return runners.AnswerContext{
		Body:           renderStructuredAnswerBody(question, hits),
		Format:         "flowcraftv2_structured_facts",
		PromptTemplate: structuredFactsAnswerPrompt,
	}
}

const maxStructuredAnswerMemories = 12

const structuredFactsAnswerPrompt = `You are answering a question using only the structured memory facts below.

Guidelines:
- Ground the answer strictly in the structured facts and evidence quotes. Do not invent facts that are not supported.
- Use EVIDENCE_PACKAGE answer_cues first, then verify against MEMORIES. The cues are extracted from the same ranked memories and are meant to reduce distraction.
- When candidate_answers are present, treat them as temporal/list/count reasoning scaffolds only; verify them against answer_cues/MEMORIES before answering.
- Each memory is listed in retrieval/rerank order as [#1], [#2], etc. Prefer lower-numbered memories when evidence conflicts, but combine compatible facts for list and bridge questions. Some lower-ranked supporting facts may appear only in EVIDENCE_PACKAGE answer_cues to keep the prompt focused.
- Treat event_time as the event date. event_time_source and event_time_text explain how that date was derived.
- Treat observed_at and evidence source_time as evidence timestamps, not event dates by themselves. Use source_time only as the anchor for relative wording in the same quote.
- If content or event_time_text uses a date qualifier ("around", "roughly", "the week before X", "a few years ago", "last summer", "two weekends ago"), preserve that qualifier when it is the best-supported answer rather than fabricating precision.
- If answer_cues includes relative_time_answer, prefer that wording for WHEN questions unless stronger primary evidence contradicts it.
- If relative_time_answer includes an ISO date, answer with that date and use the parenthetical text only as reasoning.
- Match the form of the question. If asked WHEN, give a date or duration; HOW MANY, a number; YES/NO, lead with yes/no.
- For list or set questions, extract literal named items from answer_cues and all relevant facts; return a compact comma-separated list.
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
		if i >= maxStructuredAnswerMemories {
			fmt.Fprintf(&b, "... (%d additional recalled memories omitted; use EVIDENCE_PACKAGE answer_cues for lower-ranked supporting facts.)\n", len(hits)-i)
			break
		}
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
	renderAnswerCues(b, pkg.answerCues)
	renderCandidateAnswers(b, pkg.candidateAnswers)
	b.WriteString("\n")
}

type evidencePackage struct {
	types            []string
	primaryRanks     []string
	supportingRanks  []string
	answerCues       []answerCue
	candidateAnswers []candidateAnswer
}

type answerCue struct {
	Rank               string
	Kind               string
	Content            string
	Subject            string
	Predicate          string
	Object             string
	Location           string
	Entities           []string
	Participants       []string
	EventTime          string
	EventTimeSource    string
	EventTimeText      string
	RelativeTimeAnswer string
	ObservedAt         string
	SourceTime         string
	Quote              string
}

type candidateAnswer struct {
	Type       string
	Value      string
	Source     string
	Rank       string
	Support    string
	EventTime  string
	SourceTime string
}

func buildEvidencePackage(query string, hits []recall.Hit) evidencePackage {
	types := evidencePackageLabels(query)
	if len(hits) == 0 {
		return evidencePackage{}
	}
	primaryCount := min(5, len(hits))
	pkg := evidencePackage{types: types}
	for i := 0; i < primaryCount; i++ {
		pkg.primaryRanks = append(pkg.primaryRanks, rankLabel(i))
	}
	supporting := supportingEvidenceRanks(query, types, hits, primaryCount)
	for _, rank := range supporting {
		pkg.supportingRanks = append(pkg.supportingRanks, rankLabel(rank))
	}
	for _, rank := range packageCueRanks(primaryCount, supporting) {
		pkg.answerCues = append(pkg.answerCues, answerCueFromHit(rank, hits[rank]))
	}
	pkg.candidateAnswers = candidateAnswersForQuery(query, pkg.types, pkg.answerCues)
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
	return append([]string{"direct"}, nonEmptyStrings(labels)...)
}

func supportingEvidenceRanks(query string, types []string, hits []recall.Hit, primaryCount int) []int {
	if primaryCount <= 0 || primaryCount >= len(hits) {
		return nil
	}
	setIntent := containsString(types, "set_completion")
	bridgeIntent := containsString(types, "bridge_chain")
	temporalIntent := containsString(types, "temporal_anchor")
	primary := hits[:primaryCount]
	out := make([]int, 0, len(hits)-primaryCount)
	for i := primaryCount; i < len(hits); i++ {
		hit := hits[i]
		switch {
		case setIntent && packageSetSibling(hit, primary):
			out = append(out, i)
		case bridgeIntent && packageBridgeSibling(hit, primary):
			out = append(out, i)
		case temporalIntent && packageTemporalAnchor(hit):
			out = append(out, i)
		case packageDirectCueCandidate(query, hit, primary):
			out = append(out, i)
		}
	}
	return out
}

func packageCueRanks(primaryCount int, supporting []int) []int {
	out := make([]int, 0, primaryCount+len(supporting))
	seen := map[int]struct{}{}
	for i := 0; i < primaryCount; i++ {
		seen[i] = struct{}{}
		out = append(out, i)
	}
	for _, rank := range supporting {
		if _, ok := seen[rank]; ok {
			continue
		}
		seen[rank] = struct{}{}
		out = append(out, rank)
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

func packageDirectCueCandidate(query string, hit recall.Hit, primary []recall.Hit) bool {
	tokens := packageQueryTokens(query)
	if len(tokens) == 0 {
		return false
	}
	text := strings.ToLower(strings.Join([]string{
		hit.Fact.Content,
		hit.Fact.Subject,
		hit.Fact.Predicate,
		hit.Fact.Object,
		hit.Fact.Location,
		strings.Join(hit.Fact.Entities, " "),
		strings.Join(hit.Fact.Participants, " "),
	}, " "))
	matches := 0
	for token := range tokens {
		if strings.Contains(text, token) {
			matches++
		}
	}
	if matches >= 2 {
		return true
	}
	for _, existing := range primary {
		if packageFactSibling(hit.Fact, existing.Fact) && matches >= 1 {
			return true
		}
	}
	return false
}

func packageQueryTokens(query string) map[string]struct{} {
	raw := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	out := map[string]struct{}{}
	for _, token := range raw {
		if len(token) < 3 || packageQueryStopword(token) {
			continue
		}
		out[token] = struct{}{}
	}
	return out
}

func packageQueryStopword(token string) bool {
	switch token {
	case "the", "and", "for", "with", "what", "when", "where", "which", "who", "whom", "why", "how", "did", "does", "was", "were", "are", "has", "have", "had", "would", "could", "should", "about", "from", "into", "that", "this", "their", "there", "then", "than", "his", "her", "she", "him", "you", "your":
		return true
	default:
		return false
	}
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

func answerCueFromHit(idx int, hit recall.Hit) answerCue {
	f := hit.Fact
	ref := firstAnswerCueEvidence(hit)
	cue := answerCue{
		Rank:         rankLabel(idx),
		Kind:         string(f.Kind),
		Content:      compactAnswerCue(f.Content),
		Subject:      f.Subject,
		Predicate:    f.Predicate,
		Object:       f.Object,
		Location:     f.Location,
		Entities:     f.Entities,
		Participants: f.Participants,
		Quote:        compactAnswerCue(ref.Text),
	}
	if f.ValidFrom != nil && !f.ValidFrom.IsZero() && rendersEventTime(f.Metadata) {
		cue.EventTime = f.ValidFrom.Format("2006-01-02")
		cue.EventTimeSource = metadataString(f.Metadata, validFromSourceMetadataKey)
		cue.EventTimeText = metadataString(f.Metadata, "valid_from_text")
	}
	if !f.ObservedAt.IsZero() {
		cue.ObservedAt = f.ObservedAt.Format("2006-01-02 15:04")
	}
	if !ref.Timestamp.IsZero() {
		cue.SourceTime = evidenceSourceTimeLabel(ref)
	}
	cue.RelativeTimeAnswer = relativeTimeAnswerCue(cue.Quote, cue.EventTimeText, cue.Content, ref.Timestamp)
	return cue
}

type answerQuestionProfile struct {
	temporal   bool
	collection bool
	count      bool
}

func candidateAnswersForQuery(query string, types []string, cues []answerCue) []candidateAnswer {
	if len(cues) == 0 {
		return nil
	}
	profile := answerQuestionProfileFor(query, types)
	out := make([]candidateAnswer, 0, len(cues))
	seen := map[string]struct{}{}
	listValues := map[string]string{}
	add := func(kind, source, value string, cue answerCue) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(kind + "\x00" + source + "\x00" + value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, candidateAnswer{
			Type:       kind,
			Value:      value,
			Source:     source,
			Rank:       cue.Rank,
			Support:    compactAnswerCue(candidateAnswerSupport(cue)),
			EventTime:  cue.EventTime,
			SourceTime: cue.SourceTime,
		})
	}
	for _, cue := range cues {
		if profile.temporal {
			if cue.RelativeTimeAnswer != "" {
				add("temporal", "relative_time", cue.RelativeTimeAnswer, cue)
			} else if cue.EventTimeText != "" {
				add("temporal", "event_time_text", cue.EventTimeText, cue)
			} else {
				add("temporal", "event_time", cue.EventTime, cue)
			}
		}
		if profile.collection {
			if !strongListCandidateCue(cue, profile) {
				continue
			}
			for _, value := range listCandidateValues(cue) {
				if _, ok := listValues[strings.ToLower(value)]; !ok {
					listValues[strings.ToLower(value)] = value
				}
				add("list_item", "object", value, cue)
			}
		}
	}
	if profile.count && len(listValues) > 0 {
		out = append(out, candidateAnswer{
			Type:    "count",
			Value:   strconv.Itoa(len(listValues)),
			Source:  "list_item_count",
			Support: "unique list_item candidate count",
		})
	}
	return out
}

func answerQuestionProfileFor(query string, types []string) answerQuestionProfile {
	phrases := phrase.New(query)
	temporal := containsString(types, "temporal_anchor") ||
		phrases.ContainsAny("when") ||
		phrases.ContainsPhrase("how", "long") ||
		phrases.ContainsAny("before", "after")
	count := phrases.ContainsPhrase("how", "many") || phrases.ContainsPhrase("how", "much")
	return answerQuestionProfile{
		temporal:   temporal,
		collection: containsString(types, "set_completion") || count,
		count:      count,
	}
}

func listCandidateValues(cue answerCue) []string {
	var values []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || containsStringFold(values, value) {
			return
		}
		values = append(values, value)
	}
	add(cue.Object)
	if cue.Object == "" {
		add(cue.Location)
		for _, participant := range cue.Participants {
			add(participant)
		}
	}
	return values
}

func strongListCandidateCue(cue answerCue, profile answerQuestionProfile) bool {
	if cue.Object == "" {
		return false
	}
	predicate := strings.ToLower(strings.TrimSpace(cue.Predicate))
	content := strings.ToLower(cue.Content + " " + cue.Quote)
	switch {
	case profile.count && (strings.Contains(predicate, "has") || strings.Contains(predicate, "own") || strings.Contains(predicate, "keep")):
		return true
	case strings.Contains(predicate, "has_") || strings.Contains(predicate, "like") || strings.Contains(predicate, "prefer"):
		return true
	case strings.Contains(content, " has ") || strings.Contains(content, " have ") || strings.Contains(content, " bought ") || strings.Contains(content, " owns "):
		return true
	default:
		return false
	}
}

func candidateAnswerSupport(cue answerCue) string {
	if cue.Content != "" {
		return cue.Content
	}
	return cue.Quote
}

type surfaceAnswerSpan struct {
	source string
	value  string
}

func surfaceAnswerSpans(cue answerCue) []surfaceAnswerSpan {
	var out []surfaceAnswerSpan
	add := func(source, value string) {
		value = strings.TrimSpace(value)
		if len([]rune(value)) < 2 || containsSurfaceAnswerSpan(out, value) {
			return
		}
		out = append(out, surfaceAnswerSpan{source: source, value: value})
	}
	for _, span := range quotedAnswerSpans(cue.Content) {
		add("content_quote_span", span)
	}
	for _, span := range quotedAnswerSpans(cue.Quote) {
		add("quote_span", span)
	}
	for _, span := range sharedImageAnswerSpans(cue.Content) {
		add("content_image_span", span)
	}
	for _, span := range sharedImageAnswerSpans(cue.Quote) {
		add("quote_image_span", span)
	}
	return out
}

func quotedAnswerSpans(text string) []string {
	var spans []string
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		quote := runes[i]
		if quote != '"' && quote != '\'' && quote != '“' && quote != '”' && quote != '‘' && quote != '’' {
			continue
		}
		for j := i + 1; j < len(runes); j++ {
			if !matchingQuote(quote, runes[j]) {
				continue
			}
			span := strings.TrimSpace(string(runes[i+1 : j]))
			if span != "" {
				spans = append(spans, span)
			}
			i = j
			break
		}
	}
	return spans
}

func matchingQuote(open, close rune) bool {
	switch open {
	case '"', '“', '”':
		return close == '"' || close == '”'
	case '\'', '‘', '’':
		return close == '\'' || close == '’'
	default:
		return false
	}
}

func sharedImageAnswerSpans(text string) []string {
	lower := strings.ToLower(text)
	var spans []string
	const marker = "[shared image:"
	start := strings.Index(lower, marker)
	for start >= 0 {
		bodyStart := start + len(marker)
		end := strings.Index(text[bodyStart:], "]")
		if end < 0 {
			break
		}
		span := strings.TrimSpace(text[bodyStart : bodyStart+end])
		if span != "" {
			spans = append(spans, span)
		}
		nextStart := bodyStart + end + 1
		if nextStart >= len(text) {
			break
		}
		next := strings.Index(strings.ToLower(text[nextStart:]), marker)
		if next < 0 {
			break
		}
		start = nextStart + next
	}
	return spans
}

func containsSurfaceAnswerSpan(spans []surfaceAnswerSpan, want string) bool {
	for _, span := range spans {
		if strings.EqualFold(span.value, want) {
			return true
		}
	}
	return false
}

func firstAnswerCueEvidence(hit recall.Hit) recall.EvidenceRef {
	evidence := hit.Evidence
	if len(evidence) == 0 {
		evidence = hit.Fact.EvidenceRefs
	}
	for _, ref := range evidence {
		if strings.TrimSpace(ref.Text) != "" || !ref.Timestamp.IsZero() {
			return ref
		}
	}
	if len(evidence) > 0 {
		return evidence[0]
	}
	return recall.EvidenceRef{}
}

func compactAnswerCue(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	runes := []rune(value)
	if len(runes) <= 260 {
		return value
	}
	return strings.TrimSpace(string(runes[:257])) + "..."
}

func relativeTimeAnswerCue(quote, eventTimeText, content string, sourceTime time.Time) string {
	if sourceTime.IsZero() {
		return ""
	}
	text := strings.ToLower(strings.Join([]string{quote, eventTimeText, content}, " "))
	anchor := sourceTime.Format("2006-01-02")
	switch {
	case strings.Contains(text, "last friday"):
		day := previousWeekday(sourceTime, time.Friday)
		return day.Format("2006-01-02") + " (the Friday before " + anchor + ")"
	case strings.Contains(text, "last week"):
		return "the week before " + anchor
	case strings.Contains(text, "last month"):
		return "the month before " + anchor
	case strings.Contains(text, "last year"):
		return "the year before " + anchor
	case strings.Contains(text, "yesterday"):
		day := sourceTime.AddDate(0, 0, -1)
		return day.Format("2006-01-02") + " (the day before " + anchor + ")"
	case strings.Contains(text, "two days ago"):
		day := sourceTime.AddDate(0, 0, -2)
		return day.Format("2006-01-02") + " (two days before " + anchor + ")"
	case strings.Contains(text, "two weeks ago"):
		return "two weeks before " + anchor
	case strings.Contains(text, "few weeks ago") || strings.Contains(text, "a few weeks ago"):
		return "a few weeks before " + anchor
	case strings.Contains(text, "two weekends ago"):
		return "two weekends before " + anchor
	default:
		return ""
	}
}

func previousWeekday(anchor time.Time, weekday time.Weekday) time.Time {
	days := (int(anchor.Weekday()) - int(weekday) + 7) % 7
	if days == 0 {
		days = 7
	}
	return anchor.AddDate(0, 0, -days)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsStringFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
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

func renderAnswerCues(b *strings.Builder, cues []answerCue) {
	if len(cues) == 0 {
		return
	}
	writeIndent(b, 1)
	b.WriteString("answer_cues:\n")
	for _, cue := range cues {
		writeIndent(b, 2)
		b.WriteString("-\n")
		writeKV(b, 3, "rank", cue.Rank)
		writeKV(b, 3, "kind", cue.Kind)
		writeKV(b, 3, "content", cue.Content)
		writeKV(b, 3, "subject", cue.Subject)
		writeKV(b, 3, "predicate", cue.Predicate)
		writeKV(b, 3, "object", cue.Object)
		writeKV(b, 3, "location", cue.Location)
		writeListKV(b, 3, "entities", cue.Entities)
		writeListKV(b, 3, "participants", cue.Participants)
		writeKV(b, 3, "event_time", cue.EventTime)
		writeKV(b, 3, "event_time_source", cue.EventTimeSource)
		writeKV(b, 3, "event_time_text", cue.EventTimeText)
		writeKV(b, 3, "relative_time_answer", cue.RelativeTimeAnswer)
		writeKV(b, 3, "observed_at", cue.ObservedAt)
		writeKV(b, 3, "source_time", cue.SourceTime)
		writeKV(b, 3, "quote", cue.Quote)
	}
}

func renderCandidateAnswers(b *strings.Builder, answers []candidateAnswer) {
	if len(answers) == 0 {
		return
	}
	writeIndent(b, 1)
	b.WriteString("candidate_answers:\n")
	for _, answer := range answers {
		writeIndent(b, 2)
		b.WriteString("-\n")
		writeKV(b, 3, "type", answer.Type)
		writeKV(b, 3, "value", answer.Value)
		writeKV(b, 3, "source", answer.Source)
		writeKV(b, 3, "rank", answer.Rank)
		writeKV(b, 3, "event_time", answer.EventTime)
		writeKV(b, 3, "source_time", answer.SourceTime)
		writeKV(b, 3, "support", answer.Support)
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
