package ingest

import (
	"fmt"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"strings"
)

func guardedParameterProposal(p ParameterProposal, reason string) diagnostic.GuardedSemanticProposal {
	return diagnostic.GuardedSemanticProposal{
		Content:     strings.TrimSpace(p.NameSurface + " " + p.ValueSurface),
		Family:      string(proposalFamilyParameter),
		Kind:        string(proposalFamilyParameter),
		Subject:     strings.TrimSpace(p.Owner),
		Predicate:   strings.TrimSpace(firstNonEmpty(p.OperationSurface, p.OperatorSurface)),
		Object:      strings.TrimSpace(p.ValueSurface),
		SourceIDs:   cleanSourceIDs(p.SourceIDs),
		Quote:       strings.TrimSpace(p.Quote),
		GuardReason: strings.TrimSpace(reason),
	}
}

func compileParameterTemporalFact(promotion promotionDecision) domain.TemporalFact {
	grounded := promotion.Grounded
	p := grounded.Proposal.Parameter
	if p == nil {
		return domain.TemporalFact{}
	}
	nameSurface := strings.TrimSpace(p.NameSurface)
	valueSurface := strings.TrimSpace(p.ValueSurface)
	normalized := grounded.Normalized
	content := renderParameterContent(normalized.Owner, normalized.NamespacePath, normalized.CanonicalName, normalized.Operation, normalized.DisplayValue, normalized.Unit, normalized.ConstraintOperator, normalized.ConditionSurface)
	refs := appendEvidenceRefs(grounded.SupportRefs, grounded.ConfirmationRefs)
	meta := map[string]any{
		domain.MetaAssertionFamily:             "parameter",
		domain.MetaParameterOwner:              normalized.Owner,
		domain.MetaParameterNamespacePath:      normalized.NamespacePath,
		domain.MetaParameterNameSurface:        nameSurface,
		domain.MetaParameterCanonicalName:      normalized.CanonicalName,
		domain.MetaParameterOperation:          normalized.Operation,
		domain.MetaParameterValueKind:          normalized.ValueKind,
		domain.MetaParameterRawValue:           valueSurface,
		domain.MetaParameterNormalizedValue:    normalized.NormalizedValue,
		domain.MetaParameterUnit:               normalized.Unit,
		domain.MetaParameterCondition:          normalized.ConditionSurface,
		domain.MetaParameterConstraintOperator: normalized.ConstraintOperator,
		domain.MetaParameterGroundingLevel:     string(grounded.Level),
		domain.MetaParameterSupportSpanIDs:     normalized.SupportSpanIDs,
		domain.MetaParameterNormalizationTrace: normalized.Trace,
	}
	return domain.TemporalFact{
		Kind:             domain.KindParameter,
		Content:          content,
		Subject:          normalized.Owner,
		Predicate:        "parameter_value",
		Object:           normalized.DisplayValue,
		Entities:         parameterEntities(normalized.Owner, normalized.NamespacePath, normalized.CanonicalName, normalized.DisplayValue),
		EvidenceRefs:     refs,
		SourceMessageIDs: sourceIDsFromEvidence(refs),
		EvidenceText:     evidenceTextFromRefs(refs, nil),
		Metadata:         meta,
	}
}

func CanonicalizeParameterFact(f domain.TemporalFact) (domain.TemporalFact, error) {
	if f.Kind != domain.KindParameter {
		return f, nil
	}
	if len(f.EvidenceRefs) == 0 {
		return domain.TemporalFact{}, fmt.Errorf("canonical observation/span evidence required")
	}
	for _, ref := range f.EvidenceRefs {
		if strings.TrimSpace(ref.ObservationID) == "" || strings.TrimSpace(ref.SpanID) == "" {
			return domain.TemporalFact{}, fmt.Errorf("canonical observation/span evidence required")
		}
	}
	if f.Metadata == nil {
		return domain.TemporalFact{}, fmt.Errorf("parameter metadata is required")
	}
	owner := canonicalParameterOwner(metadataStringFromMap(f.Metadata, domain.MetaParameterOwner))
	if owner == "" {
		owner = canonicalParameterOwner(f.Subject)
	}
	namespace := strings.TrimSpace(metadataStringFromMap(f.Metadata, domain.MetaParameterNamespacePath))
	name := strings.TrimSpace(metadataStringFromMap(f.Metadata, domain.MetaParameterCanonicalName))
	if name == "" {
		_, name = canonicalParameterName(namespace, metadataStringFromMap(f.Metadata, domain.MetaParameterNameSurface))
	}
	valueKind := strings.TrimSpace(metadataStringFromMap(f.Metadata, domain.MetaParameterValueKind))
	normalizedValue := strings.TrimSpace(metadataStringFromMap(f.Metadata, domain.MetaParameterNormalizedValue))
	operation := strings.TrimSpace(metadataStringFromMap(f.Metadata, domain.MetaParameterOperation))
	if operation == "clear" && normalizedValue == "" {
		normalizedValue = clearedParameterValue
		if valueKind == "" {
			valueKind = clearedParameterValueKind
		}
	}
	if owner == "" || name == "" || valueKind == "" || normalizedValue == "" {
		return domain.TemporalFact{}, fmt.Errorf("parameter owner/canonical_name/value_kind/normalized_value are required")
	}
	meta := copyMetadata(f.Metadata)
	meta[domain.MetaAssertionFamily] = "parameter"
	meta[domain.MetaParameterOwner] = owner
	meta[domain.MetaParameterNamespacePath] = namespace
	meta[domain.MetaParameterCanonicalName] = name
	meta[domain.MetaParameterValueKind] = valueKind
	meta[domain.MetaParameterNormalizedValue] = normalizedValue
	if _, ok := meta[domain.MetaParameterGroundingLevel]; !ok {
		meta[domain.MetaParameterGroundingLevel] = string(groundingExact)
	}
	f.Metadata = meta
	f.Subject = owner
	f.Predicate = "parameter_value"
	if f.Object == "" {
		f.Object = normalizedValue
	}
	if f.Content == "" {
		f.Content = renderParameterContent(owner, namespace, name, metadataStringFromMap(meta, domain.MetaParameterOperation), f.Object, metadataStringFromMap(meta, domain.MetaParameterUnit), metadataStringFromMap(meta, domain.MetaParameterConstraintOperator), metadataStringFromMap(meta, domain.MetaParameterCondition))
	}
	f.SourceMessageIDs = sourceIDsFromEvidence(f.EvidenceRefs)
	if f.EvidenceText == "" {
		f.EvidenceText = evidenceTextFromRefs(f.EvidenceRefs, nil)
	}
	return f, nil
}

func ValidateParameterFactEvidenceSupport(f domain.TemporalFact, supportTexts []string) error {
	if f.Kind != domain.KindParameter {
		return nil
	}
	operation := strings.TrimSpace(metadataStringFromMap(f.Metadata, domain.MetaParameterOperation))
	nameCandidates := []string{
		metadataStringFromMap(f.Metadata, domain.MetaParameterNameSurface),
		strings.ReplaceAll(metadataStringFromMap(f.Metadata, domain.MetaParameterCanonicalName), "_", " "),
		metadataStringFromMap(f.Metadata, domain.MetaParameterCanonicalName),
	}
	valueCandidates := []string{
		metadataStringFromMap(f.Metadata, domain.MetaParameterRawValue),
		strings.TrimSpace(f.Object),
		metadataStringFromMap(f.Metadata, domain.MetaParameterNormalizedValue),
	}
	condition := metadataStringFromMap(f.Metadata, domain.MetaParameterCondition)
	var sawName bool
	var sawValue bool
	var sawCondition bool
	for _, text := range supportTexts {
		nameOK := anySurfaceSupportedByText(text, nameCandidates...)
		if nameOK {
			sawName = true
		}
		valueOK := operation == "clear" || anySurfaceSupportedByText(text, valueCandidates...)
		if valueOK && operation != "clear" {
			sawValue = true
		}
		conditionOK := strings.TrimSpace(condition) == "" || anySurfaceSupportedByText(text, condition)
		if conditionOK && strings.TrimSpace(condition) != "" {
			sawCondition = true
		}
		if nameOK && valueOK && conditionOK {
			return nil
		}
	}
	if !sawName {
		return fmt.Errorf("parameter name is not supported by evidence")
	}
	if operation != "clear" && !sawValue {
		return fmt.Errorf("parameter value is not supported by evidence")
	}
	if strings.TrimSpace(condition) != "" && !sawCondition {
		return fmt.Errorf("parameter condition is not supported by evidence")
	}
	return fmt.Errorf("parameter name/value/condition are not supported by the same evidence span")
}

func anySurfaceSupportedByText(text string, candidates ...string) bool {
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if containsSurface(text, candidate) {
			return true
		}
	}
	return false
}

func copyMetadata(meta map[string]any) map[string]any {
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	return out
}
