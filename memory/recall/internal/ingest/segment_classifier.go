package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"strings"
)

const SegmentClassificationSchema = `{
  "type": "object",
  "properties": {
    "segments": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "segment_id": {"type": "string"},
          "families": {
            "type": "array",
            "items": {"type": "string", "enum": ["semantic_fact", "parameter_slot"]}
          }
        },
        "required": ["segment_id", "families"],
        "additionalProperties": false
      }
    }
  },
  "required": ["segments"],
  "additionalProperties": false
}`

type classifiedSegments map[semanticProposalFamily][]domain.SourceEvidenceSpan

type segmentClassificationReply struct {
	Segments []segmentFamilyRoute `json:"segments"`
}

type segmentFamilyRoute struct {
	SegmentID string   `json:"segment_id"`
	Families  []string `json:"families"`
}

const SemanticClassifierSystemPrompt = `You route canonical evidence segments to typed proposal extractors.

Return no facts and no proposal fields. Routing is deliberately conservative:
when a segment may contain a family, route it to that family. Do not extract
values, names, or facts in this stage. Procedure and intent families are
reserved and must not be emitted.`

func (e *LLMExtractor) classifySegments(ctx context.Context, input port.IngestInput, segments []evidenceSegment) (classifiedSegments, error) {
	spans := make([]domain.SourceEvidenceSpan, 0, len(segments))
	byID := make(map[string][]domain.SourceEvidenceSpan, len(segments))
	addAlias := func(alias string, span domain.SourceEvidenceSpan) {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			return
		}
		byID[alias] = append(byID[alias], span)
	}
	for _, segment := range segments {
		id := evidenceSegmentID(segment.Span)
		if id == "" {
			continue
		}
		spans = append(spans, segment.Span)
		byID[id] = []domain.SourceEvidenceSpan{segment.Span}
		for _, alias := range []string{segment.Span.SourceID, segment.Span.SpanID, segment.Span.ObservationID} {
			addAlias(alias, segment.Span)
		}
	}
	if len(spans) == 0 {
		return nil, nil
	}
	reply, usage, err := e.Client.Generate(ctx,
		[]llm.Message{
			llm.NewTextMessage(llm.RoleSystem, SemanticClassifierSystemPrompt),
			llm.NewTextMessage(llm.RoleUser, buildExtractorInputEnvelope(input, buildExtractorSourceEvidenceJSONL(spans))),
		},
		llm.WithJSONSchema(llm.JSONSchemaParam{
			Name:   schemaNameForFamily(e.SchemaName, "segment_classifier"),
			Schema: json.RawMessage(SegmentClassificationSchema),
			Strict: true,
		}),
		llm.WithJSONMode(true),
	)
	recordExtractorTokenUsage(ctx, "segment_classifier", usage)
	if err != nil {
		return nil, fmt.Errorf("recall extractor segment_classifier: llm: %w", err)
	}
	jsonBytes, _, err := llm.ExtractJSON(reply.Content())
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("recall extractor segment_classifier: extract llm json: %w", err))
	}
	var parsed segmentClassificationReply
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		return nil, errdefs.Validation(fmt.Errorf("recall extractor segment_classifier: parse llm json: %w", err))
	}
	routes := classifiedSegments{}
	for _, segment := range parsed.Segments {
		matchedSpans, ok := byID[strings.TrimSpace(segment.SegmentID)]
		if !ok {
			continue
		}
		for _, family := range segment.Families {
			normalized := semanticProposalFamily(proposalFamily(family))
			if normalized == "" || !activeProposalFamily(normalized) {
				continue
			}
			routes[normalized] = append(routes[normalized], matchedSpans...)
		}
	}
	return routes, nil
}
