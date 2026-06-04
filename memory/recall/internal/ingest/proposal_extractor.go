package ingest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"strings"
	"sync"
)

type typedExtractorSpec struct {
	Family     semanticProposalFamily
	Prompt     string
	SchemaName string
	Schema     string
}

const defaultMaxConcurrentTypedExtractors = 8

func typedExtractorSpecs(baseName string) []typedExtractorSpec {
	return []typedExtractorSpec{
		{
			Family:     proposalFamilySemanticFact,
			Prompt:     SemanticFactProposalSystemPrompt,
			SchemaName: schemaNameForFamily(baseName, "semantic_fact"),
			Schema:     SemanticFactProposalSchema,
		},
		{
			Family:     proposalFamilyParameter,
			Prompt:     ParameterProposalSystemPrompt,
			SchemaName: schemaNameForFamily(baseName, "parameter_slot"),
			Schema:     ParameterProposalSchema,
		},
	}
}

func schemaNameForFamily(baseName, family string) string {
	baseName = strings.TrimSpace(baseName)
	if baseName == "" {
		baseName = "recall_semantic_proposals"
	}
	return baseName + "_" + family
}

func (e *LLMExtractor) extractTypedProposals(ctx context.Context, input port.IngestInput, routes classifiedSegments) ([]proposalCandidate, error) {
	specs := typedExtractorSpecs(e.SchemaName)
	type result struct {
		family    semanticProposalFamily
		proposals []proposalCandidate
		err       error
	}
	jobCount := 0
	for _, spec := range specs {
		jobCount += len(routes[spec.Family])
	}
	if jobCount == 0 {
		return nil, nil
	}
	results := make(chan result, jobCount)
	sem := make(chan struct{}, e.maxConcurrentTypedExtractors())
	var wg sync.WaitGroup
	for _, spec := range specs {
		spans := routes[spec.Family]
		if len(spans) == 0 {
			continue
		}
		for _, span := range spans {
			wg.Add(1)
			go func(spec typedExtractorSpec, span domain.SourceEvidenceSpan) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					results <- result{family: spec.Family, err: ctx.Err()}
					return
				}
				proposals, err := e.extractTypedProposalFamily(ctx, input, spec, []domain.SourceEvidenceSpan{span})
				results <- result{family: spec.Family, proposals: proposals, err: err}
			}(spec, span)
		}
	}
	wg.Wait()
	close(results)
	var out []proposalCandidate
	for res := range results {
		if res.err != nil {
			return nil, res.err
		}
		out = append(out, res.proposals...)
	}
	return out, nil
}

func (e *LLMExtractor) maxConcurrentTypedExtractors() int {
	if e != nil && e.MaxConcurrentExtractors > 0 {
		return e.MaxConcurrentExtractors
	}
	return defaultMaxConcurrentTypedExtractors
}

func (e *LLMExtractor) extractTypedProposalFamily(ctx context.Context, input port.IngestInput, spec typedExtractorSpec, spans []domain.SourceEvidenceSpan) ([]proposalCandidate, error) {
	evidenceJSONL := buildExtractorSourceEvidenceJSONL(spans)
	if strings.TrimSpace(evidenceJSONL) == "" {
		return nil, nil
	}
	userMessage := buildExtractorInputEnvelope(input, evidenceJSONL)
	system := spec.Prompt
	messages := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, system),
		llm.NewTextMessage(llm.RoleUser, userMessage),
	}
	opts := []llm.GenerateOption{
		llm.WithJSONSchema(llm.JSONSchemaParam{
			Name:   spec.SchemaName,
			Schema: json.RawMessage(spec.Schema),
			Strict: true,
		}),
		llm.WithJSONMode(true),
	}
	if e.Temperature != 0 {
		opts = append(opts, llm.WithTemperature(e.Temperature))
	}
	opts = append(opts, e.ExtraOptions...)

	reply, usage, err := e.Client.Generate(ctx, messages, opts...)
	recordExtractorTokenUsage(ctx, string(spec.Family), usage)
	if err != nil {
		return nil, fmt.Errorf("recall extractor %s: llm: %w", spec.Family, err)
	}
	body := reply.Content()
	if body == "" {
		return nil, nil
	}
	jsonBytes, _, err := llm.ExtractJSON(body)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("recall extractor %s: extract llm json: %w", spec.Family, err))
	}
	parsed, err := parseTypedProposalReply(jsonBytes, spec.Family)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("recall extractor %s: parse llm json: %w", spec.Family, err))
	}
	if typedProposalOverflow(jsonBytes) {
		recordExtractorCandidateRejected(ctx, diagnostic.GuardedSemanticProposal{
			Family:      string(spec.Family),
			Kind:        string(spec.Family),
			GuardReason: "segment_overflow",
		})
	}
	for i := range parsed {
		parsed[i].SourceSpans = append([]domain.SourceEvidenceSpan(nil), spans...)
	}
	return assignProposalIDs(parsed), nil
}

func typedProposalOverflow(body []byte) bool {
	var envelope struct {
		Overflow bool `json:"overflow"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}
	return envelope.Overflow
}

func parseTypedProposalReply(body []byte, family semanticProposalFamily) ([]proposalCandidate, error) {
	switch family {
	case proposalFamilyParameter:
		return parseParameterProposalReply(body)
	default:
		return parseSemanticFactProposalReply(body, family)
	}
}
