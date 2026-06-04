package ingest

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

type groundingLevel string

const (
	groundingExact             groundingLevel = "exact"
	groundingNormalized        groundingLevel = "normalized"
	groundingComposed          groundingLevel = "composed"
	groundingDialogueConfirmed groundingLevel = "dialogue_confirmed"
	groundingInferred          groundingLevel = "inferred"
	groundingAmbiguous         groundingLevel = "ambiguous"
	groundingUnsupported       groundingLevel = "unsupported"
)

type groundingResult struct {
	Accepted []groundedProposal
	Rejected []groundedProposal
}

type groundedProposal struct {
	ProposalID       string
	Family           semanticProposalFamily
	Proposal         proposalCandidate
	SupportRefs      []domain.EvidenceRef
	ConfirmationRefs []domain.EvidenceRef
	Level            groundingLevel
	Normalized       groundedNormalizedFields
	SemanticBinding  groundedSemanticBinding
	SemanticContent  string
	RejectReason     string
}

type groundedSemanticBinding struct {
	Text             string
	EvidenceText     string
	ParameterOverlap *groundedParameterOverlapBinding
}

type groundedParameterOverlapBinding struct {
	Owner             string
	NamespacePath     string
	CanonicalName     string
	ValueKind         string
	NormalizedValue   string
	ConditionIdentity string
}

type groundedNormalizedFields struct {
	Owner              string
	NamespacePath      string
	CanonicalName      string
	Operation          string
	ConstraintOperator string
	ValueKind          string
	RawValue           string
	NormalizedValue    string
	DisplayValue       string
	Unit               string
	ConditionSurface   string
	ConditionIdentity  string
	SupportSpanIDs     []string
	Trace              map[string]any
}

func groundProposals(ctx context.Context, proposals []proposalCandidate, sourceSpans []domain.SourceEvidenceSpan) groundingResult {
	var result groundingResult
	for _, proposal := range proposals {
		grounded, ok, reason := groundProposal(proposal, sourceSpans)
		if !ok {
			grounded.ProposalID = proposal.ID
			grounded.Family = proposal.Family
			grounded.Proposal = proposal
			grounded.RejectReason = reason
			result.Rejected = append(result.Rejected, grounded)
			recordRejectedProposal(ctx, proposal, reason)
			continue
		}
		result.Accepted = append(result.Accepted, grounded)
	}
	return result
}

func groundProposal(proposal proposalCandidate, sourceSpans []domain.SourceEvidenceSpan) (groundedProposal, bool, string) {
	switch proposal.Family {
	case proposalFamilyParameter:
		if proposal.Parameter == nil {
			return groundedProposal{}, false, "unsupported_schema"
		}
		return groundParameterCandidate(proposal, sourceSpans)
	default:
		if proposal.Semantic == nil {
			return groundedProposal{}, false, "unsupported_schema"
		}
		return groundSemanticCandidate(proposal)
	}
}

func groundParameterCandidate(proposal proposalCandidate, extractableSpans []domain.SourceEvidenceSpan) (groundedProposal, bool, string) {
	p := *proposal.Parameter
	nameSurface := strings.TrimSpace(p.NameSurface)
	valueSurface := strings.TrimSpace(p.ValueSurface)
	if nameSurface == "" {
		return groundedProposal{}, false, "name_not_grounded"
	}
	initialOperation := resolveParameterOperation(p.OperationSurface, p.OperatorSurface)
	if initialOperation.Ambiguous {
		return groundedProposal{}, false, "operation_ambiguous"
	}
	if valueSurface == "" && initialOperation.Operation != "clear" {
		return groundedProposal{}, false, "value_not_grounded"
	}
	if len(proposal.SourceSpans) == 0 {
		return groundedProposal{}, false, "graph_dependencies_missing"
	}
	ground, ok, reason := groundParameterProposal(p, proposal.SourceSpans, extractableSpans)
	if !ok {
		return groundedProposal{}, false, reason
	}
	surfaces, ok, reason := groundedParameterSurfaces(p, ground)
	if !ok {
		return groundedProposal{}, false, reason
	}
	owner := canonicalParameterOwner(firstNonEmpty(surfaces.Owner, ground.Speaker))
	namespace, canonicalName := canonicalParameterName("", nameSurface)
	operationResult := resolveParameterOperation(surfaces.Operation, surfaces.Operator)
	if operationResult.Ambiguous {
		return groundedProposal{}, false, "operation_ambiguous"
	}
	operation := operationResult.Operation
	constraint := canonicalConstraintOperator(firstNonEmpty(surfaces.Operator, surfaces.Operation))
	if constraint != "" {
		operation = "constrain"
	}
	normalized := normalizeParameterValueForOperation(valueSurface, p.NormalizedValueHint, operation)
	refs := []domain.EvidenceRef{{
		ID:            ground.SourceID,
		MessageID:     ground.SourceID,
		ObservationID: ground.ObservationID,
		SpanID:        ground.SpanID,
		SessionID:     ground.SessionID,
		Role:          ground.Role,
		Speaker:       ground.Speaker,
		Text:          ground.Text,
		Timestamp:     ground.Timestamp,
	}}
	confirmationRefs := []domain.EvidenceRef(nil)
	supportSpanIDs := []string{ground.SpanID}
	if ground.Confirmation != nil {
		ref := domain.EvidenceRef{
			ID:            ground.Confirmation.SourceID,
			MessageID:     ground.Confirmation.SourceID,
			ObservationID: ground.Confirmation.ObservationID,
			SpanID:        ground.Confirmation.SpanID,
			SessionID:     ground.Confirmation.SessionID,
			Role:          ground.Confirmation.Role,
			Speaker:       ground.Confirmation.Speaker,
			Text:          ground.Confirmation.Text,
			Timestamp:     ground.Confirmation.Timestamp,
		}
		confirmationRefs = append(confirmationRefs, ref)
		supportSpanIDs = append(supportSpanIDs, ground.Confirmation.SpanID)
	}
	return groundedProposal{
		ProposalID:       proposal.ID,
		Family:           proposal.Family,
		Proposal:         proposal,
		SupportRefs:      refs,
		ConfirmationRefs: confirmationRefs,
		Level:            groundingLevel(ground.Level),
		Normalized: groundedNormalizedFields{
			Owner:              owner,
			NamespacePath:      namespace,
			CanonicalName:      canonicalName,
			Operation:          operation,
			ConstraintOperator: constraint,
			ValueKind:          normalized.kind,
			RawValue:           valueSurface,
			NormalizedValue:    normalized.value,
			DisplayValue:       normalized.display,
			Unit:               normalized.unit,
			ConditionSurface:   surfaces.Condition,
			ConditionIdentity:  canonicalParameterCondition(surfaces.Condition),
			SupportSpanIDs:     supportSpanIDs,
			Trace:              normalized.trace,
		},
	}, true, ""
}

func groundSemanticCandidate(proposal proposalCandidate) (groundedProposal, bool, string) {
	m := *proposal.Semantic
	refs, reason := semanticProposalEvidenceRefsWithReason(m.SourceIDs, m.Quote, proposal.SourceSpans)
	if reason != "" {
		return groundedProposal{}, false, reason
	}
	binding, ok, reason := groundSemanticBinding(m, refs)
	if !ok {
		return groundedProposal{}, false, reason
	}
	return groundedProposal{
		ProposalID:      proposal.ID,
		Family:          proposal.Family,
		Proposal:        proposal,
		SupportRefs:     refs,
		Level:           groundingExact,
		SemanticBinding: binding,
		SemanticContent: binding.Text,
	}, true, ""
}

func groundSemanticBinding(m SemanticFactProposal, refs []domain.EvidenceRef) (groundedSemanticBinding, bool, string) {
	if !hasEvidenceID(refs) {
		return groundedSemanticBinding{}, false, "no_evidence"
	}
	evidenceText := semanticContentFromEvidence(refs)
	if isTrivialExtractedContent(evidenceText) {
		return groundedSemanticBinding{}, false, "empty_evidence"
	}
	if _, ok := selfContainedExtractedContent(evidenceText); !ok {
		return groundedSemanticBinding{}, false, "non_self_contained_evidence"
	}
	text := strings.TrimSpace(m.Text)
	if isTrivialExtractedContent(text) {
		return groundedSemanticBinding{}, false, "empty_text"
	}
	text, ok := selfContainedExtractedContent(text)
	if !ok {
		return groundedSemanticBinding{}, false, "non_self_contained"
	}
	if !semanticProposalTextBoundToEvidence(text, evidenceText) {
		return groundedSemanticBinding{}, false, "text_not_grounded"
	}
	return groundedSemanticBinding{
		Text:             text,
		EvidenceText:     evidenceText,
		ParameterOverlap: semanticParameterOverlapBinding(m, text),
	}, true, ""
}

func semanticProposalTextBoundToEvidence(text, evidenceText string) bool {
	textNorm := normalizedSemanticBindingText(text)
	evidenceNorm := normalizedSemanticBindingText(evidenceText)
	if textNorm == "" || evidenceNorm == "" {
		return false
	}
	return textNorm == evidenceNorm || strings.Contains(evidenceNorm, textNorm)
}

func normalizedSemanticBindingText(text string) string {
	text = strings.TrimSpace(text)
	text = strings.Trim(text, " \t\r\n.。!！")
	replacer := strings.NewReplacer(
		"=", " is ",
		"==", " is ",
		":", " is ",
		"，", " ",
		",", " ",
		"。", " ",
		".", " ",
	)
	return normalizeEvidenceQuote(replacer.Replace(text))
}

func semanticParameterOverlapBinding(m SemanticFactProposal, content string) *groundedParameterOverlapBinding {
	value := normalizeParameterValue(m.Object, "")
	if strings.TrimSpace(value.value) == "" || !containsSurface(content, m.Object) {
		return nil
	}
	for _, candidate := range semanticParameterNameCandidates(m, content) {
		namespace, name := canonicalParameterName("", candidate.name)
		if name == "" {
			continue
		}
		return &groundedParameterOverlapBinding{
			Owner:           canonicalParameterOwner(candidate.owner),
			NamespacePath:   namespace,
			CanonicalName:   name,
			ValueKind:       value.kind,
			NormalizedValue: value.value,
		}
	}
	return nil
}

type semanticParameterNameCandidate struct {
	owner string
	name  string
}

func semanticParameterNameCandidates(m SemanticFactProposal, content string) []semanticParameterNameCandidate {
	var out []semanticParameterNameCandidate
	add := func(owner, name string) {
		owner = strings.TrimSpace(owner)
		name = strings.TrimSpace(name)
		if name == "" || !containsSurface(content, name) {
			return
		}
		if owner != "" && !containsSurface(content, owner) {
			return
		}
		out = append(out, semanticParameterNameCandidate{owner: owner, name: name})
	}
	for _, name := range semanticParameterNameSuffixes(m.Predicate) {
		add(m.Subject, name)
	}
	add("", m.Subject)
	add(m.Subject, m.Predicate)
	return out
}

func semanticParameterNameSuffixes(surface string) []string {
	fields := strings.Fields(strings.TrimSpace(surface))
	if len(fields) < 2 {
		return nil
	}
	out := make([]string, 0, len(fields)-1)
	for i := 1; i < len(fields); i++ {
		out = append(out, strings.Join(fields[i:], " "))
	}
	return out
}

func cleanSourceIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func semanticProposalEvidenceRefsWithReason(sourceIDs []string, quote string, spans []domain.SourceEvidenceSpan) ([]domain.EvidenceRef, string) {
	if len(spans) == 0 {
		return nil, "graph_dependencies_missing"
	}
	ids := cleanSourceIDs(sourceIDs)
	if len(ids) == 0 {
		return nil, "no_evidence"
	}
	quote = strings.TrimSpace(quote)
	if quote == "" {
		return nil, "quote_required"
	}
	var sawSource bool
	var out []domain.EvidenceRef
	var sawQuote bool
	for _, id := range ids {
		var bestRef *domain.EvidenceRef
		var bestSpan domain.SourceEvidenceSpan
		for _, span := range spans {
			if !sourceIDMatches(span, []string{id}) {
				continue
			}
			sawSource = true
			located, ok := locateQuoteInText(span.Text, quote)
			if !ok {
				continue
			}
			sawQuote = true
			ref := domain.EvidenceRef{
				ID:            span.SourceID,
				MessageID:     span.SourceID,
				ObservationID: span.ObservationID,
				SpanID:        span.SpanID,
				SessionID:     span.SessionID,
				Role:          span.Role,
				Speaker:       span.Speaker,
				Text:          located,
				Timestamp:     span.Timestamp,
			}
			if bestRef == nil || betterEvidenceSpan(span, bestSpan, located, bestRef.Text) {
				bestRef = &ref
				bestSpan = span
			}
		}
		if bestRef != nil {
			out = append(out, *bestRef)
		}
	}
	if len(out) > 0 {
		return dedupeDomainEvidenceRefs(out), ""
	}
	if sawSource {
		if !sawQuote {
			return nil, "quote_not_in_source"
		}
		return nil, "quote_not_in_source"
	}
	return nil, "unknown_source_id"
}

func betterEvidenceSpan(candidate, current domain.SourceEvidenceSpan, candidateText, currentText string) bool {
	if current.SpanID == "" {
		return true
	}
	candidateLen := len(strings.TrimSpace(candidateText))
	currentLen := len(strings.TrimSpace(currentText))
	if currentLen == 0 || (candidateLen > 0 && candidateLen < currentLen) {
		return true
	}
	if candidateLen != currentLen {
		return false
	}
	return spanSpecificity(candidate) > spanSpecificity(current)
}

func dedupeDomainEvidenceRefs(refs []domain.EvidenceRef) []domain.EvidenceRef {
	seen := map[string]struct{}{}
	out := make([]domain.EvidenceRef, 0, len(refs))
	for _, ref := range refs {
		key := evidenceRefDedupeKey(ref.ID, ref.MessageID, ref.Text)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ref)
	}
	return out
}

type parameterGrounding struct {
	Level         string
	ObservationID string
	SpanID        string
	SourceID      string
	SessionID     string
	Role          string
	Speaker       string
	Text          string
	SpanText      string
	Timestamp     time.Time
	Confirmation  *domain.SourceEvidenceSpan
}

type parameterSurfaces struct {
	Owner     string
	Operation string
	Operator  string
	Condition string
}

func groundedParameterSurfaces(p ParameterProposal, ground parameterGrounding) (parameterSurfaces, bool, string) {
	text := firstNonEmpty(ground.SpanText, ground.Text)
	out := parameterSurfaces{}
	if owner := strings.TrimSpace(p.Owner); owner != "" && containsSurface(text, owner) {
		out.Owner = owner
	}
	if condition := strings.TrimSpace(p.ConditionSurface); condition != "" {
		if !containsSurface(text, condition) {
			return parameterSurfaces{}, false, "condition_not_grounded"
		}
		out.Condition = condition
	}
	if operator := strings.TrimSpace(p.OperatorSurface); operator != "" {
		if !containsSurface(text, operator) {
			return parameterSurfaces{}, false, "operator_not_grounded"
		}
		out.Operator = operator
	}
	if operation := strings.TrimSpace(p.OperationSurface); operation != "" {
		if !containsSurface(text, operation) {
			return parameterSurfaces{}, false, "operation_not_grounded"
		} else {
			out.Operation = operation
		}
	}
	return out, true, ""
}

func groundParameterProposal(p ParameterProposal, spans, extractableSpans []domain.SourceEvidenceSpan) (parameterGrounding, bool, string) {
	ids := cleanSourceIDs(p.SourceIDs)
	if len(ids) == 0 {
		return parameterGrounding{}, false, "no_evidence"
	}
	quote := strings.TrimSpace(p.Quote)
	if quote == "" {
		return parameterGrounding{}, false, "quote_required"
	}
	name := strings.TrimSpace(p.NameSurface)
	value := strings.TrimSpace(p.ValueSurface)
	operation := normalizeParameterOperation(p.OperationSurface, p.OperatorSurface)
	requiresConfirmation := operation == "confirm" || parameterProposalHasConfirmationEvidence(p)
	var sawSource bool
	var sawQuote bool
	var best *parameterGrounding
	for _, span := range spans {
		if !sourceIDMatches(span, ids) {
			continue
		}
		sawSource = true
		text := span.Text
		located, ok := locateQuoteInText(text, quote)
		if !ok {
			continue
		}
		sawQuote = true
		supportText := located
		nameOK := containsSurface(supportText, name)
		valueMatch := matchParameterValue(supportText, value)
		if !nameOK {
			continue
		}
		if !valueMatch.OK {
			return parameterGrounding{}, false, "value_not_grounded"
		}
		if !parameterPairingGrounded(supportText, name, valueMatch, operation) {
			return parameterGrounding{}, false, "ambiguous_pairing"
		}
		level := "exact"
		if valueMatch.Normalized {
			level = "normalized"
		}
		candidate := parameterGrounding{
			Level:         level,
			ObservationID: span.ObservationID,
			SpanID:        span.SpanID,
			SourceID:      span.SourceID,
			SessionID:     span.SessionID,
			Role:          span.Role,
			Speaker:       span.Speaker,
			Text:          supportText,
			SpanText:      span.Text,
			Timestamp:     span.Timestamp,
		}
		if best == nil || betterParameterGrounding(candidate, *best, span, spans) {
			best = &candidate
		}
	}
	if best != nil {
		if requiresConfirmation {
			confirmation, ok := findConfirmationEvidenceSpan(extractableSpans, *best, p.ConfirmationSourceIDs, p.ConfirmationQuote)
			if !ok {
				return parameterGrounding{}, false, "requires_extractable_context"
			}
			best.Level = "dialogue_confirmed"
			best.Confirmation = &confirmation
		}
		return *best, true, ""
	}
	if sawSource {
		if !sawQuote {
			return parameterGrounding{}, false, "quote_not_in_source"
		}
		return parameterGrounding{}, false, "name_not_grounded"
	}
	return parameterGrounding{}, false, "unknown_source_id"
}

func parameterProposalHasConfirmationEvidence(p ParameterProposal) bool {
	return len(cleanSourceIDs(p.ConfirmationSourceIDs)) > 0 || strings.TrimSpace(p.ConfirmationQuote) != ""
}

func betterParameterGrounding(candidate, current parameterGrounding, candidateSpan domain.SourceEvidenceSpan, allSpans []domain.SourceEvidenceSpan) bool {
	if candidate.Level == "exact" && current.Level != "exact" {
		return true
	}
	if candidate.Level != "exact" && current.Level == "exact" {
		return false
	}
	currentLen := len(current.Text)
	candidateLen := len(candidate.Text)
	if currentLen == 0 || (candidateLen > 0 && candidateLen < currentLen) {
		return true
	}
	if candidateLen != currentLen {
		return false
	}
	return spanSpecificity(candidateSpan) > spanSpecificity(sourceSpanByID(allSpans, current.SpanID))
}

func sourceSpanByID(spans []domain.SourceEvidenceSpan, spanID string) domain.SourceEvidenceSpan {
	for _, span := range spans {
		if span.SpanID == spanID {
			return span
		}
	}
	return domain.SourceEvidenceSpan{}
}

func spanSpecificity(span domain.SourceEvidenceSpan) int {
	switch span.Kind {
	case domain.ObservationSpanKindTableRow, domain.ObservationSpanKindListItem, domain.ObservationSpanKindSentence:
		return 4
	case domain.ObservationSpanKindParagraph:
		return 3
	case domain.ObservationSpanKindQuote:
		return 2
	case domain.ObservationSpanKindTurn, domain.ObservationSpanKindText:
		return 1
	default:
		return 0
	}
}

func sourceIDMatches(span domain.SourceEvidenceSpan, ids []string) bool {
	for _, id := range ids {
		if id == span.SourceID || id == span.SpanID || id == span.ObservationID || id == evidenceSegmentID(span) {
			return true
		}
	}
	return false
}

func locateQuoteInText(text, quote string) (string, bool) {
	if quote == "" {
		return text, true
	}
	if idx := strings.Index(text, quote); idx >= 0 {
		return text[idx : idx+len(quote)], true
	}
	return tokenEquivalentQuoteSpan(text, quote)
}

func containsSurface(text, surface string) bool {
	text = strings.TrimSpace(text)
	surface = strings.TrimSpace(surface)
	if surface == "" {
		return true
	}
	if strings.Contains(text, surface) {
		return true
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(surface))
}

type parameterValueMatch struct {
	OK         bool
	Normalized bool
	Surface    string
}

func matchParameterValue(text, value string) parameterValueMatch {
	value = strings.TrimSpace(value)
	if value == "" {
		return parameterValueMatch{OK: true}
	}
	if surface, ok := locateSurfaceInText(text, value); ok {
		return parameterValueMatch{OK: true, Surface: surface}
	}
	return parameterValueMatch{}
}

func locateSurfaceInText(text, surface string) (string, bool) {
	text = strings.TrimSpace(text)
	surface = strings.TrimSpace(surface)
	if surface == "" {
		return "", false
	}
	if idx := strings.Index(text, surface); idx >= 0 {
		return text[idx : idx+len(surface)], true
	}
	lowerText := strings.ToLower(text)
	lowerSurface := strings.ToLower(surface)
	if idx := strings.Index(lowerText, lowerSurface); idx >= 0 {
		return text[idx : idx+len(surface)], true
	}
	return "", false
}

func parameterPairingGrounded(text, name string, value parameterValueMatch, operation string) bool {
	if strings.TrimSpace(operation) == "clear" || strings.TrimSpace(value.Surface) == "" {
		return true
	}
	if ok, paired := parallelListPairing(text, name, value.Surface); ok {
		return paired
	}
	_, nameEnd, ok := surfaceBounds(text, name)
	if !ok {
		return false
	}
	valueStart, _, ok := surfaceBounds(text, value.Surface)
	if !ok {
		return false
	}
	if valueStart < nameEnd {
		return false
	}
	between := text[nameEnd:valueStart]
	if strings.ContainsAny(between, ",，;；\n") {
		return false
	}
	if strings.ContainsAny(between, "=:：") || strings.Contains(between, "->") {
		return true
	}
	return strings.TrimSpace(between) == ""
}

func surfaceBounds(text, surface string) (int, int, bool) {
	surface = strings.TrimSpace(surface)
	if surface == "" {
		return 0, 0, false
	}
	if idx := strings.Index(text, surface); idx >= 0 {
		return idx, idx + len(surface), true
	}
	lowerText := strings.ToLower(text)
	lowerSurface := strings.ToLower(surface)
	if idx := strings.Index(lowerText, lowerSurface); idx >= 0 {
		return idx, idx + len(surface), true
	}
	return 0, 0, false
}

func parallelListPairing(text, name, value string) (bool, bool) {
	for _, sep := range []string{"=", "：", ":"} {
		idx := strings.Index(text, sep)
		if idx <= 0 {
			continue
		}
		left := splitParallelParts(text[:idx])
		right := splitParallelParts(text[idx+len(sep):])
		if len(left) <= 1 || len(right) <= 1 {
			continue
		}
		if len(left) != len(right) {
			return true, false
		}
		nameIdx := indexContaining(left, name)
		valueIdx := indexContaining(right, value)
		if nameIdx < 0 || valueIdx < 0 {
			continue
		}
		return true, nameIdx == valueIdx
	}
	return false, false
}

func splitParallelParts(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == '；'
	})
	out := fields[:0]
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

func indexContaining(parts []string, surface string) int {
	for i, part := range parts {
		if containsSurface(part, surface) {
			return i
		}
	}
	return -1
}

type quoteToken struct {
	text      string
	startByte int
	endByte   int
}

func tokenEquivalentQuoteSpan(text, quote string) (string, bool) {
	textTokens := quoteTokens(text)
	quoteTokens := quoteTokens(quote)
	if len(textTokens) == 0 || len(quoteTokens) == 0 || len(quoteTokens) > len(textTokens) {
		return "", false
	}
	for i := 0; i <= len(textTokens)-len(quoteTokens); i++ {
		matched := true
		for j := range quoteTokens {
			if textTokens[i+j].text != quoteTokens[j].text {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		start := textTokens[i].startByte
		end := textTokens[i+len(quoteTokens)-1].endByte
		return text[start:end], true
	}
	return "", false
}

func quoteTokens(s string) []quoteToken {
	var out []quoteToken
	tokenStart := -1
	var token strings.Builder
	flush := func(end int) {
		if tokenStart < 0 {
			return
		}
		out = append(out, quoteToken{
			text:      token.String(),
			startByte: tokenStart,
			endByte:   end,
		})
		token.Reset()
		tokenStart = -1
	}
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if tokenStart < 0 {
				tokenStart = i
			}
			token.WriteRune(unicode.ToLower(r))
			continue
		}
		flush(i)
	}
	flush(len(s))
	return out
}

func normalizeEvidenceQuote(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

// evidenceRefDedupeKey produces the canonical dedupe key for an
// EvidenceRef. Prefer ID over MessageID over normalized text so two
// refs that share an id but differ slightly on quoted text still
// collapse to one canonical ref.
func evidenceRefDedupeKey(id, messageID, text string) string {
	if id != "" {
		return "id:" + id
	}
	if messageID != "" {
		return "msg:" + messageID
	}
	return "text:" + strings.ToLower(strings.Join(strings.Fields(text), " "))
}
