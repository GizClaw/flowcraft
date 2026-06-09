package indexed

import (
	"encoding/json"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type sourceRefDTO struct {
	Kind     string                `json:"kind"`
	Message  *messageSourceRefDTO  `json:"message,omitempty"`
	Document *documentSourceRefDTO `json:"document,omitempty"`
}

type messageSourceRefDTO struct {
	ConversationID string   `json:"conversation_id"`
	MessageID      string   `json:"message_id"`
	Span           *spanDTO `json:"span,omitempty"`
}

type documentSourceRefDTO struct {
	DatasetID   string   `json:"dataset_id"`
	DocumentID  string   `json:"document_id"`
	Version     string   `json:"version,omitempty"`
	ContentHash string   `json:"content_hash,omitempty"`
	Span        *spanDTO `json:"span,omitempty"`
}

type spanDTO struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type signatureDTO struct {
	ViewID               string               `json:"view_id,omitempty"`
	SourceRevisions      []sourceRevisionDTO  `json:"source_revisions,omitempty"`
	UpstreamViewRefs     []upstreamViewRefDTO `json:"upstream_view_refs,omitempty"`
	TransformSignature   string               `json:"transform_signature,omitempty"`
	DiagnosticSignatures map[string]string    `json:"diagnostic_signatures,omitempty"`
}

type sourceRevisionDTO struct {
	Kind        string `json:"kind"`
	SourceKey   string `json:"source_key"`
	Revision    string `json:"revision,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	ObservedAt  string `json:"observed_at,omitempty"`
}

type upstreamViewRefDTO struct {
	ViewID          string `json:"view_id"`
	OutputSignature string `json:"output_signature"`
	RecordKey       string `json:"record_key,omitempty"`
}

// DecodeSourceRefs decodes indexed source refs from metadata. It accepts both
// the DTO values produced by Writer and JSON round-tripped map/slice values.
func DecodeSourceRefs(metadata map[string]any) ([]views.SourceRef, bool, error) {
	value, ok := metadata[MetadataSourceRefsKey]
	if !ok {
		return nil, false, nil
	}

	var dtos []sourceRefDTO
	if err := decodeDTO(value, &dtos); err != nil {
		return nil, true, errdefs.Validationf("%s: decode source refs: %w", errPrefix, err)
	}
	refs := make([]views.SourceRef, len(dtos))
	for i, dto := range dtos {
		refs[i] = sourceRefFromDTO(dto)
		if err := refs[i].Validate(); err != nil {
			return nil, true, errdefs.Validationf("%s: invalid source ref: %w", errPrefix, err)
		}
	}
	return refs, true, nil
}

// DecodeSignature decodes an indexed view signature from metadata. It accepts
// both the DTO value produced by Writer and JSON round-tripped map values.
func DecodeSignature(metadata map[string]any) (views.ViewSignature, bool, error) {
	value, ok := metadata[MetadataSignatureKey]
	if !ok {
		return views.ViewSignature{}, false, nil
	}

	var dto signatureDTO
	if err := decodeDTO(value, &dto); err != nil {
		return views.ViewSignature{}, true, errdefs.Validationf("%s: decode signature: %w", errPrefix, err)
	}
	signature, err := signatureFromDTO(dto)
	if err != nil {
		return views.ViewSignature{}, true, err
	}
	if err := signature.Validate(); err != nil {
		return views.ViewSignature{}, true, errdefs.Validationf("%s: invalid signature: %w", errPrefix, err)
	}
	return signature, true, nil
}

func sourceRefsToDTO(refs []views.SourceRef) []sourceRefDTO {
	if refs == nil {
		return nil
	}
	out := make([]sourceRefDTO, len(refs))
	for i, ref := range refs {
		out[i] = sourceRefToDTO(ref)
	}
	return out
}

func sourceRefToDTO(ref views.SourceRef) sourceRefDTO {
	dto := sourceRefDTO{Kind: string(ref.Kind)}
	if ref.Message != nil {
		dto.Message = &messageSourceRefDTO{
			ConversationID: ref.Message.ConversationID,
			MessageID:      ref.Message.MessageID,
			Span:           spanToDTO(ref.Message.Span),
		}
	}
	if ref.Document != nil {
		dto.Document = &documentSourceRefDTO{
			DatasetID:   ref.Document.DatasetID,
			DocumentID:  ref.Document.DocumentID,
			Version:     ref.Document.Version,
			ContentHash: ref.Document.ContentHash,
			Span:        spanToDTO(ref.Document.Span),
		}
	}
	return dto
}

func sourceRefFromDTO(dto sourceRefDTO) views.SourceRef {
	ref := views.SourceRef{Kind: views.SourceKind(dto.Kind)}
	if dto.Message != nil {
		ref.Message = &views.MessageSourceRef{
			ConversationID: dto.Message.ConversationID,
			MessageID:      dto.Message.MessageID,
			Span:           spanFromDTO(dto.Message.Span),
		}
	}
	if dto.Document != nil {
		ref.Document = &views.DocumentSourceRef{
			DatasetID:   dto.Document.DatasetID,
			DocumentID:  dto.Document.DocumentID,
			Version:     dto.Document.Version,
			ContentHash: dto.Document.ContentHash,
			Span:        spanFromDTO(dto.Document.Span),
		}
	}
	return ref
}

func spanToDTO(span *views.Span) *spanDTO {
	if span == nil {
		return nil
	}
	return &spanDTO{Start: span.Start, End: span.End}
}

func spanFromDTO(dto *spanDTO) *views.Span {
	if dto == nil {
		return nil
	}
	return &views.Span{Start: dto.Start, End: dto.End}
}

func signatureToDTO(signature views.ViewSignature) signatureDTO {
	dto := signatureDTO{
		ViewID:             string(signature.ViewID),
		TransformSignature: signature.TransformSignature,
	}
	if signature.SourceRevisions != nil {
		dto.SourceRevisions = make([]sourceRevisionDTO, len(signature.SourceRevisions))
		for i, rev := range signature.SourceRevisions {
			dto.SourceRevisions[i] = sourceRevisionToDTO(rev)
		}
	}
	if signature.UpstreamViewRefs != nil {
		dto.UpstreamViewRefs = make([]upstreamViewRefDTO, len(signature.UpstreamViewRefs))
		for i, ref := range signature.UpstreamViewRefs {
			dto.UpstreamViewRefs[i] = upstreamViewRefToDTO(ref)
		}
	}
	if signature.DiagnosticSignatures != nil {
		dto.DiagnosticSignatures = cloneStringMap(signature.DiagnosticSignatures)
	}
	return dto
}

func sourceRevisionToDTO(rev views.SourceRevision) sourceRevisionDTO {
	dto := sourceRevisionDTO{
		Kind:        string(rev.Kind),
		SourceKey:   rev.SourceKey,
		Revision:    rev.Revision,
		ContentHash: rev.ContentHash,
	}
	if !rev.ObservedAt.IsZero() {
		dto.ObservedAt = rev.ObservedAt.UTC().Format(time.RFC3339Nano)
	}
	return dto
}

func upstreamViewRefToDTO(ref views.UpstreamViewRef) upstreamViewRefDTO {
	return upstreamViewRefDTO{
		ViewID:          string(ref.ViewID),
		OutputSignature: ref.OutputSignature,
		RecordKey:       ref.RecordKey,
	}
}

func signatureFromDTO(dto signatureDTO) (views.ViewSignature, error) {
	signature := views.ViewSignature{
		ViewID:             views.ID(dto.ViewID),
		TransformSignature: dto.TransformSignature,
	}
	if dto.SourceRevisions != nil {
		signature.SourceRevisions = make([]views.SourceRevision, len(dto.SourceRevisions))
		for i, revDTO := range dto.SourceRevisions {
			rev, err := sourceRevisionFromDTO(revDTO)
			if err != nil {
				return views.ViewSignature{}, err
			}
			signature.SourceRevisions[i] = rev
		}
	}
	if dto.UpstreamViewRefs != nil {
		signature.UpstreamViewRefs = make([]views.UpstreamViewRef, len(dto.UpstreamViewRefs))
		for i, refDTO := range dto.UpstreamViewRefs {
			signature.UpstreamViewRefs[i] = views.UpstreamViewRef{
				ViewID:          views.ID(refDTO.ViewID),
				OutputSignature: refDTO.OutputSignature,
				RecordKey:       refDTO.RecordKey,
			}
		}
	}
	if dto.DiagnosticSignatures != nil {
		signature.DiagnosticSignatures = cloneStringMap(dto.DiagnosticSignatures)
	}
	return signature, nil
}

func sourceRevisionFromDTO(dto sourceRevisionDTO) (views.SourceRevision, error) {
	rev := views.SourceRevision{
		Kind:        views.SourceKind(dto.Kind),
		SourceKey:   dto.SourceKey,
		Revision:    dto.Revision,
		ContentHash: dto.ContentHash,
	}
	if dto.ObservedAt != "" {
		observedAt, err := time.Parse(time.RFC3339Nano, dto.ObservedAt)
		if err != nil {
			return views.SourceRevision{}, errdefs.Validationf("%s: invalid source revision observed_at: %w", errPrefix, err)
		}
		rev.ObservedAt = observedAt
	}
	return rev, nil
}

func decodeDTO(value any, out any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
