package entity

import (
	"maps"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/fact"
)

func cloneProfileRecord(in ProfileRecord) ProfileRecord {
	out := in
	out.Slots = cloneSlots(in.Slots)
	out.FactRefs = cloneFactRefs(in.FactRefs)
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	if in.Metadata != nil {
		out.Metadata = cloneAnyMap(in.Metadata)
	}
	return out
}

func cloneProfileRecords(in []ProfileRecord) []ProfileRecord {
	if in == nil {
		return nil
	}
	out := make([]ProfileRecord, len(in))
	for i, record := range in {
		out[i] = cloneProfileRecord(record)
	}
	return out
}

func cloneSlots(in []Slot) []Slot {
	if in == nil {
		return nil
	}
	out := make([]Slot, len(in))
	for i, slot := range in {
		out[i] = slot
		out[i].FactRefs = cloneFactRefs(slot.FactRefs)
		if slot.Metadata != nil {
			out[i].Metadata = cloneAnyMap(slot.Metadata)
		}
	}
	return out
}

func cloneEvent(in Event) Event {
	out := in
	out.OccurredAt = cloneTimePtr(in.OccurredAt)
	out.ValidFrom = cloneTimePtr(in.ValidFrom)
	out.ValidUntil = cloneTimePtr(in.ValidUntil)
	out.FactRefs = cloneFactRefs(in.FactRefs)
	out.SourceRefs = cloneSourceRefs(in.SourceRefs)
	out.Signature = cloneViewSignature(in.Signature)
	if in.Metadata != nil {
		out.Metadata = cloneAnyMap(in.Metadata)
	}
	return out
}

func cloneEvents(in []Event) []Event {
	if in == nil {
		return nil
	}
	out := make([]Event, len(in))
	for i, event := range in {
		out[i] = cloneEvent(event)
	}
	return out
}

func cloneTimePtr(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneFactRefs(in []fact.FactRef) []fact.FactRef {
	if in == nil {
		return nil
	}
	return append([]fact.FactRef(nil), in...)
}

func cloneSourceRefs(in []views.SourceRef) []views.SourceRef {
	if in == nil {
		return nil
	}
	out := make([]views.SourceRef, len(in))
	for i, ref := range in {
		out[i] = cloneSourceRef(ref)
	}
	return out
}

func cloneSourceRef(ref views.SourceRef) views.SourceRef {
	if ref.Message != nil {
		msg := *ref.Message
		if msg.Span != nil {
			span := *msg.Span
			msg.Span = &span
		}
		ref.Message = &msg
	}
	if ref.Document != nil {
		doc := *ref.Document
		if doc.Span != nil {
			span := *doc.Span
			doc.Span = &span
		}
		ref.Document = &doc
	}
	return ref
}

func cloneViewSignature(in views.ViewSignature) views.ViewSignature {
	out := in
	if in.SourceRevisions != nil {
		out.SourceRevisions = append([]views.SourceRevision(nil), in.SourceRevisions...)
	}
	if in.UpstreamViewRefs != nil {
		out.UpstreamViewRefs = append([]views.UpstreamViewRef(nil), in.UpstreamViewRefs...)
	}
	if in.DiagnosticSignatures != nil {
		out.DiagnosticSignatures = maps.Clone(in.DiagnosticSignatures)
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return cloneAnyMap(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = cloneAny(item)
		}
		return out
	default:
		return v
	}
}
