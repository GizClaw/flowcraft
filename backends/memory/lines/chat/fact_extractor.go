// Package chat implements chat-line fact derivation.
package chat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

const (
	// KindRawMessage is the canonical message-source input kind.
	KindRawMessage component.ArtifactKind = "raw_message"
	// KindFact is the extracted fact output kind.
	KindFact component.ArtifactKind = "fact"

	// AlgorithmVersion participates in derivation policy identity. Bump it
	// whenever the extractor prompt or parsing logic changes: prompt text is
	// not part of the policy digest, so without a bump existing workspaces
	// would keep facts derived by the previous prompt.
	// 1.3.0: the response schema requires every property (strict structured
	// outputs reject optional keys), so the prompt now spells out [] and "" for
	// absent details. Existing workspaces must re-derive.
	// 1.4.0: the worked examples were rewritten. The previous ones were lifted
	// from a benchmark conversation -- names, dates and the "LGBTQ support
	// group" fact came from LoCoMo, and one example even carried a gold answer
	// string -- so the prompt was partly tuned to one dataset. The examples now
	// describe the same rules with invented people and dates. Existing
	// workspaces must re-derive.
	AlgorithmVersion          = "1.4.0"
	LinkAlgorithmVersion      = "fact-link-vector-v2"
	CanonicalAlgorithmVersion = factview.CanonicalAlgorithmVersion
	TransformSignatureSimple  = "fact-extract-simple-v1"
	TransformSignatureRich    = "fact-extract-rich-v1"
)

//go:embed prompts/fact_system.tmpl
var factPromptFS embed.FS

var (
	factSystemTmpl = template.Must(template.ParseFS(factPromptFS, "prompts/fact_system.tmpl"))
	factSystem     = mustRenderFactSystem(false)
	// The rich schema asks for two fields the simple one does not, so the
	// prompt has to describe them: a strict provider would otherwise reject
	// the request or invent values for keys nobody explained.
	factSystemRich = mustRenderFactSystem(true)
)

func mustRenderFactSystem(rich bool) string {
	var builder strings.Builder
	if err := factSystemTmpl.Execute(&builder, struct{ Rich bool }{Rich: rich}); err != nil {
		panic(err)
	}
	return strings.TrimSpace(builder.String())
}

// factSystemFor selects the prompt that matches the strategy's response
// schema.
func factSystemFor(strategy FactStrategy) string {
	if strategy == StrategyRich {
		return factSystemRich
	}
	return factSystem
}

// FactStrategy controls extraction detail without changing the one-call batch contract.
type FactStrategy string

const (
	StrategyNone   FactStrategy = "none"
	StrategySimple FactStrategy = "simple"
	StrategyRich   FactStrategy = "rich"
)

// Runtime is the narrow inference surface fact extraction needs: unary
// generate for extraction and model inspection for the optional embedding
// link path. *inference.Assembly satisfies it.
type Runtime interface {
	Generate(context.Context, model.ModelRef, inference.GenerateRequest) (inference.GenerateResponse, error)
	Embed(context.Context, model.ModelRef, inference.EmbedRequest) (inference.EmbedResponse, error)
	InspectModel(model.ModelRef) (model.ModelDescriptor, error)
}

// Config makes all extraction and association bounds explicit.
type Config struct {
	Strategy               FactStrategy
	TailMaxChars           int
	MaxFacts               int
	MaxFactChars           int
	MaxQueryChars          int
	MaxEmbeddingInputChars int
	Runtime                Runtime
	GenerateModel          *model.ModelRef
	EmbedModel             *model.ModelRef
	Facts                  *factview.FactStore
	LinkVectorSearcher     component.VectorSearcher
}

// DefaultConfig returns the bounded default simple strategy.
func DefaultConfig() Config {
	return Config{
		Strategy: StrategySimple, TailMaxChars: 15000, MaxFacts: 64,
		MaxFactChars: 2000, MaxQueryChars: 2000, MaxEmbeddingInputChars: 8000,
	}
}

// Hard caps on extraction inputs and outputs. A derived fact is a
// model-visible retrieval item, so its size must stay bounded no matter how
// the host configures the extractor.
const (
	MaxTailChars           = 200_000
	MaxFacts               = 512
	MaxFactChars           = 8_000
	MaxQueryChars          = 8_000
	MaxEmbeddingInputChars = 64_000
)

// Validate rejects unknown strategies and out-of-range caps.
func (config Config) Validate() error {
	switch config.Strategy {
	case StrategyNone, StrategySimple, StrategyRich:
	default:
		return fmt.Errorf("chat line: unsupported fact strategy %q", config.Strategy)
	}
	for name, bound := range map[string]struct{ value, max int }{
		"tail_max_chars":            {config.TailMaxChars, MaxTailChars},
		"max_facts":                 {config.MaxFacts, MaxFacts},
		"max_fact_chars":            {config.MaxFactChars, MaxFactChars},
		"max_query_chars":           {config.MaxQueryChars, MaxQueryChars},
		"max_embedding_input_chars": {config.MaxEmbeddingInputChars, MaxEmbeddingInputChars},
	} {
		if bound.value <= 0 {
			return fmt.Errorf("chat line: %s must be positive", name)
		}
		if bound.value > bound.max {
			return fmt.Errorf("chat line: %s must not exceed %d", name, bound.max)
		}
	}
	if config.GenerateModel != nil {
		if err := config.GenerateModel.Validate(); err != nil {
			return fmt.Errorf("chat line: generate model: %w", err)
		}
	}
	if config.EmbedModel != nil {
		if err := config.EmbedModel.Validate(); err != nil {
			return fmt.Errorf("chat line: embed model: %w", err)
		}
	}
	return nil
}

// FactExtractor batches one source artifact into at most one Generate call.
type FactExtractor struct {
	config Config
}

type modelFact struct {
	Text           string   `json:"text"`
	Fact           string   `json:"fact,omitempty"`
	Entities       []string `json:"entities,omitempty"`
	Predicate      string   `json:"predicate,omitempty"`
	TemporalDetail string   `json:"temporal_detail,omitempty"`
	EventTime      string   `json:"event_time,omitempty"`
}

type factBatch struct {
	Facts []modelFact `json:"facts"`
}

var _ component.Deriver = (*FactExtractor)(nil)

// NewFactExtractor preserves the original constructor with default simple policy.
func NewFactExtractor(runtime Runtime, model *model.ModelRef) (*FactExtractor, error) {
	config := DefaultConfig()
	config.Runtime, config.GenerateModel = runtime, model
	return NewFactExtractorWithConfig(config)
}

// NewFactExtractorWithConfig constructs a bounded strategy-aware extractor.
func NewFactExtractorWithConfig(config Config) (*FactExtractor, error) {
	if config.Strategy == "" {
		defaults := DefaultConfig()
		config.Strategy = defaults.Strategy
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.Strategy != StrategyNone {
		if config.Runtime == nil {
			return nil, errors.New("chat line: inference runtime is required")
		}
		if config.GenerateModel == nil {
			return nil, errors.New("chat line: inference model is required")
		}
	}
	return &FactExtractor{config: config}, nil
}

func (extractor *FactExtractor) Derive(ctx context.Context, input component.Artifact) ([]component.Artifact, error) {
	if extractor == nil {
		return nil, errors.New("chat line: fact extractor is required")
	}
	if ctx == nil {
		return nil, errors.New("chat line: context is required")
	}
	if input.Kind != KindRawMessage {
		return nil, fmt.Errorf("chat line: input kind %q, want %q", input.Kind, KindRawMessage)
	}
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("chat line: input: %w", err)
	}
	if extractor.config.Strategy == StrategyNone {
		return []component.Artifact{}, nil
	}

	content := tailRunes(input.Content.Text(), extractor.config.TailMaxChars)
	schema := true
	request := extractionRequest(content, schema, extractor.config.Strategy)
	response, err := extractor.config.Runtime.Generate(ctx, *extractor.config.GenerateModel, request)
	if err != nil && inference.IsKind(err, inference.UnsupportedFeature) {
		// DeepSeek and other json_object-only providers reject schema
		// constrained responses at compile time. Retry once with
		// json_object; the lenient decoder below still enforces the fact
		// shape.
		schema = false
		response, err = extractor.config.Runtime.Generate(
			ctx, *extractor.config.GenerateModel, extractionRequest(content, schema, extractor.config.Strategy),
		)
	}
	if err != nil {
		return nil, fmt.Errorf("chat line: generate facts: %w", err)
	}
	decoded, err := decodeFacts([]byte(response.Message.Content.Text()), schema)
	if err != nil {
		return nil, fmt.Errorf("chat line: decode facts: %w", err)
	}
	if len(decoded.Facts) > extractor.config.MaxFacts {
		// Truncate instead of failing: a chatty generation would otherwise
		// fail the commit deterministically and replay forever.
		decoded.Facts = decoded.Facts[:extractor.config.MaxFacts]
	}

	eventTime := artifactEventTime(input)
	sourceDigest := digestSources(input.Sources)
	signature := TransformSignatureSimple
	if extractor.config.Strategy == StrategyRich {
		signature = TransformSignatureRich
	}
	seen := make(map[string]struct{}, len(decoded.Facts))
	output := make([]component.Artifact, 0, len(decoded.Facts))
	for _, item := range decoded.Facts {
		text := factview.NormalizeText(item.Text)
		if text == "" {
			// json_object-only providers may name the field "fact" instead
			// of "text" when the prompt contract is not enforced by a
			// response schema; accept it as a lenient alias.
			text = factview.NormalizeText(item.Fact)
		}
		if text == "" {
			continue
		}
		// Over-long facts are truncated rather than rejected; the cap is a
		// bound, not a validation contract with the model.
		text = limitRunes(text, extractor.config.MaxFactChars)
		hash := CanonicalFactHash(text)
		if _, duplicate := seen[hash]; duplicate {
			continue
		}
		seen[hash] = struct{}{}
		entities := factview.NormalizeEntities(item.Entities)
		kept := entities[:0]
		for _, entity := range entities {
			if utf8.RuneCountInString(entity) > extractor.config.MaxFactChars {
				// A truncated entity is a wrong entity: drop it.
				continue
			}
			kept = append(kept, entity)
		}
		entities = kept
		itemTime := eventTime
		if strings.TrimSpace(item.EventTime) != "" {
			parsed, parseErr := parseEventTime(strings.TrimSpace(item.EventTime))
			if parseErr != nil {
				itemTime = eventTime
			} else {
				itemTime = parsed.UTC()
			}
		}
		metadata := cloneMetadata(input.Metadata)
		metadata["canonical_hash"] = hash
		metadata["entities"] = encodeStrings(entities)
		// Absent stays absent: the fact view defaults a missing timestamp to
		// the record's creation time, which is honest, where the epoch is a
		// claim the extractor cannot support.
		if !itemTime.IsZero() {
			metadata["event_time"] = itemTime.Format(time.RFC3339Nano)
		} else {
			delete(metadata, "event_time")
		}
		metadata["source_digest"] = sourceDigest
		metadata["transform_signature"] = signature
		if extractor.config.Strategy == StrategyRich {
			metadata["predicate"] = factview.NormalizeText(item.Predicate)
			metadata["temporal_detail"] = factview.NormalizeText(item.TemporalDetail)
		}
		output = append(output, component.Artifact{
			Kind: KindFact, ID: factID(hash),
			Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: text}}},
			Sources: append([]corememory.SourceRef(nil), input.Sources...), Metadata: metadata,
		})
	}
	if err := extractor.associate(ctx, input, output); err != nil {
		return nil, err
	}
	return output, nil
}

func extractionRequest(content string, schema bool, strategy FactStrategy) inference.GenerateRequest {
	intent := inference.Intent{Text: &inference.TextIntent{}}
	if schema {
		intent.Text.Response = &inference.ResponseFormat{
			Kind: inference.ResponseJSONSchema, Name: "facts", Schema: schemaFor(strategy),
		}
	} else {
		intent.Text.Response = &inference.ResponseFormat{Kind: inference.ResponseJSONObject}
	}
	return inference.GenerateRequest{
		Context: []coremessage.Message{{
			Role:    coremessage.RoleSystem,
			Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: factSystemFor(strategy)}}},
		}},
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: content}}},
				Intent:  intent,
			},
		},
	}
}

func decodeFacts(data []byte, strict bool) (factBatch, error) {
	sanitized := sanitizeModelJSON(data)
	if strict {
		var batch factBatch
		if err := decodeStrict(sanitized, &batch); err != nil {
			return factBatch{}, err
		}
		return batch, nil
	}
	var loose struct {
		Facts  json.RawMessage `json:"facts"`
		Fact   json.RawMessage `json:"fact"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(sanitized, &loose); err != nil {
		var many []modelFact
		if arrayErr := json.Unmarshal(sanitized, &many); arrayErr == nil {
			return factBatch{Facts: many}, nil
		}
		return factBatch{}, err
	}
	if facts, ok := decodeFactList(loose.Facts); ok {
		return factBatch{Facts: facts}, nil
	}
	for _, raw := range []json.RawMessage{loose.Fact, loose.Result} {
		if facts, ok := decodeFactList(raw); ok {
			return factBatch{Facts: facts}, nil
		}
	}
	return factBatch{}, errors.New("response contains no facts array")
}

// sanitizeModelJSON extracts the outermost JSON object or array from a model
// response. Providers without a response schema routinely wrap the payload in
// prose or a Markdown code fence, and failing the whole commit for that would
// replay the same broken response on every retry.
func sanitizeModelJSON(data []byte) []byte {
	trimmed := bytes.TrimSpace(data)
	start := bytes.IndexAny(trimmed, "{[")
	if start < 0 {
		return trimmed
	}
	stack := make([]byte, 0, 8)
	inString := false
	escaped := false
	for index := start; index < len(trimmed); index++ {
		value := trimmed[index]
		if inString {
			switch {
			case escaped:
				escaped = false
			case value == '\\':
				escaped = true
			case value == '"':
				inString = false
			}
			continue
		}
		switch value {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, value)
		case '}', ']':
			if len(stack) == 0 || (stack[len(stack)-1] == '{') != (value == '}') {
				return trimmed
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return trimmed[start : index+1]
			}
		}
	}
	return trimmed
}

// decodeFactList accepts an array of facts, a single fact object, or a JSON
// string that itself encodes either shape. A present-but-empty array is a
// valid "no durable facts" result.
func decodeFactList(raw json.RawMessage) ([]modelFact, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var many []modelFact
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, true
	}
	var single modelFact
	if err := json.Unmarshal(raw, &single); err == nil && single.Text != "" {
		return []modelFact{single}, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		trimmed := strings.TrimSpace(text)
		if trimmed != "" && (strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{")) {
			return decodeFactList(json.RawMessage(trimmed))
		}
	}
	return nil, false
}

// parseEventTime accepts the RFC3339 contract plus the common date-only and
// space-separated date-time forms LLM extractors return in practice.
func parseEventTime(value string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006/01/02 15:04:05",
		"2006/01/02 15:04",
		"2006/01/02",
		"2006/1/2 15:04:05",
		"2006/1/2 15:04",
		"2006/1/2",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported event_time %q", value)
}

func (extractor *FactExtractor) associate(ctx context.Context, input component.Artifact, output []component.Artifact) error {
	if len(output) == 0 {
		return nil
	}
	scope, conversationID, ok := artifactAddress(input.Metadata)
	var existing []factview.Fact
	if ok && extractor.config.Facts != nil {
		var err error
		existing, err = extractor.config.Facts.List(ctx, scope, conversationID, factview.ListOptions{})
		if err != nil {
			return fmt.Errorf("chat line: list link candidates: %w", err)
		}
	}
	for index := range output {
		links := make(map[string]struct{})
		entities := decodeStrings(output[index].Metadata["entities"])
		eventTime, _ := time.Parse(time.RFC3339Nano, output[index].Metadata["event_time"])
		for _, candidate := range existing {
			if candidate.ID == output[index].ID ||
				candidate.CanonicalHash == output[index].Metadata["canonical_hash"] {
				for _, id := range candidate.LinkedMemoryIDs {
					links[id] = struct{}{}
				}
				continue
			}
			if overlaps(entities, candidate.Entities) || withinFiveMinutes(eventTime, candidate.EventTime) {
				links[candidate.ID] = struct{}{}
			}
		}
		for candidateIndex := range output {
			if candidateIndex == index {
				continue
			}
			candidateEntities := decodeStrings(output[candidateIndex].Metadata["entities"])
			candidateTime, _ := time.Parse(time.RFC3339Nano, output[candidateIndex].Metadata["event_time"])
			if overlaps(entities, candidateEntities) || withinFiveMinutes(eventTime, candidateTime) {
				links[output[candidateIndex].ID] = struct{}{}
			}
		}
		output[index].Metadata["linked_memory_ids"] = encodeStrings(sortedKeys(links))
	}
	return extractor.addEmbeddingLinks(ctx, scope, conversationID, ok, existing, output)
}

func (extractor *FactExtractor) addEmbeddingLinks(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	addressed bool,
	existing []factview.Fact,
	output []component.Artifact,
) error {
	if extractor.config.EmbedModel == nil || extractor.config.Runtime == nil {
		return nil
	}
	descriptor, err := extractor.config.Runtime.InspectModel(*extractor.config.EmbedModel)
	if err != nil || !supports(descriptor.Operations, model.OperationEmbed) {
		return nil
	}
	existingHashes := make(map[string]struct{}, len(existing))
	existingIDs := make(map[string]struct{}, len(existing))
	for _, fact := range existing {
		existingHashes[fact.CanonicalHash] = struct{}{}
		existingIDs[fact.ID] = struct{}{}
	}
	items := make([]inference.EmbedItem, 0, len(output))
	embeddingByHash := make(map[string]int, len(output))
	outputEmbedding := make([]int, len(output))
	for index := range outputEmbedding {
		outputEmbedding[index] = -1
	}
	for index, artifact := range output {
		hash := artifact.Metadata["canonical_hash"]
		if _, duplicate := existingHashes[hash]; duplicate {
			continue
		}
		if _, duplicate := existingIDs[artifact.ID]; duplicate {
			continue
		}
		if embeddingIndex, duplicate := embeddingByHash[hash]; duplicate {
			outputEmbedding[index] = embeddingIndex
			continue
		}
		query := limitRunes(artifact.Content.Text(), extractor.config.MaxQueryChars)
		outputEmbedding[index] = len(items)
		embeddingByHash[hash] = len(items)
		items = append(items, embedItem(limitRunes(query, extractor.config.MaxEmbeddingInputChars)))
	}
	if len(items) == 0 {
		return nil
	}
	request := inference.EmbedRequest{Items: items}
	response, err := extractor.config.Runtime.Embed(ctx, *extractor.config.EmbedModel, request)
	if err != nil {
		return fmt.Errorf("chat line: embed link candidates: %w", err)
	}
	if err := response.ValidateFor(request); err != nil {
		return fmt.Errorf("chat line: invalid link embeddings: %w", err)
	}
	for outputIndex := range output {
		links := make(map[string]struct{})
		for _, id := range decodeStrings(output[outputIndex].Metadata["linked_memory_ids"]) {
			links[id] = struct{}{}
		}
		embeddingIndex := outputEmbedding[outputIndex]
		if embeddingIndex < 0 {
			for candidateIndex, candidateEmbedding := range outputEmbedding {
				if candidateIndex != outputIndex && candidateEmbedding >= 0 {
					links[output[candidateIndex].ID] = struct{}{}
				}
			}
			delete(links, output[outputIndex].ID)
			output[outputIndex].Metadata["linked_memory_ids"] = encodeStrings(sortedKeys(links))
			continue
		}
		if addressed && extractor.config.LinkVectorSearcher != nil {
			candidates, searchErr := extractor.config.LinkVectorSearcher.SearchVector(ctx, component.VectorSearchRequest{
				Scope: scope, Vector: response.Embeddings[embeddingIndex].Vector, Limit: 5,
				Filter: component.VectorSearchFilter{
					Name: string(KindFact), Metadata: corememory.Metadata{"conversation_id": conversationID},
				},
			})
			if searchErr != nil {
				if !errdefs.IsNotFound(searchErr) {
					return fmt.Errorf("chat line: search projected link candidates: %w", searchErr)
				}
			} else {
				for _, candidate := range candidates {
					id := candidate.ID
					if candidate.Address.Kind == corememory.ContextFact && candidate.Address.ItemID != "" {
						id = candidate.Address.ItemID
					}
					links[id] = struct{}{}
				}
			}
		}
		type scored struct {
			id    string
			score float64
		}
		scores := make([]scored, 0, len(output)-1)
		for candidateIndex := range output {
			if candidateIndex == outputIndex {
				continue
			}
			candidateEmbedding := outputEmbedding[candidateIndex]
			if candidateEmbedding < 0 {
				continue
			}
			score, ok := cosine(response.Embeddings[embeddingIndex].Vector, response.Embeddings[candidateEmbedding].Vector)
			if ok {
				scores = append(scores, scored{id: output[candidateIndex].ID, score: score})
			}
		}
		sort.SliceStable(scores, func(i, j int) bool {
			if scores[i].score == scores[j].score {
				return scores[i].id < scores[j].id
			}
			return scores[i].score > scores[j].score
		})
		if len(scores) > 5 {
			scores = scores[:5]
		}
		for _, item := range scores {
			links[item.id] = struct{}{}
		}
		delete(links, output[outputIndex].ID)
		output[outputIndex].Metadata["linked_memory_ids"] = encodeStrings(sortedKeys(links))
	}
	return nil
}

// CanonicalFactHash exposes the canonical identity contract to callers.
func CanonicalFactHash(text string) string { return factview.CanonicalHash(text) }

func schemaFor(strategy FactStrategy) json.RawMessage {
	properties := `"text":{"type":"string"},"entities":{"type":"array","items":{"type":"string"}},"event_time":{"type":"string"}`
	// Every property is required, including the ones a turn may leave empty:
	// strict structured outputs (OpenAI's json_schema mode; the same rule is
	// spreading) reject a schema whose optional properties are absent from
	// required. A missing detail is expressed as [] or "", never as an omitted
	// key, and the prompt says so. DeepSeek accepts both shapes, so this also
	// keeps one schema for every provider.
	required := `["text","entities","event_time"]`
	if strategy == StrategyRich {
		properties += `,"predicate":{"type":"string"},"temporal_detail":{"type":"string"}`
		required = `["text","entities","event_time","predicate","temporal_detail"]`
	}
	return json.RawMessage(`{"type":"object","additionalProperties":false,"required":["facts"],"properties":{"facts":{"type":"array","items":{"type":"object","additionalProperties":false,"required":` + required + `,"properties":{` + properties + `}}}}}`)
}

func tailRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[len(runes)-max:])
}

func limitRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}

func factID(hash string) string {
	parts := strings.SplitN(hash, ":", 2)
	return "fact-" + parts[len(parts)-1]
}

// artifactEventTime returns the artifact's authoritative time, or the zero
// time when it carries none. A missing timestamp must stay missing: the epoch
// is a real instant, so defaulting to it made untimestamped facts look
// simultaneous (the five-minute linking window) and pushed them to the decay
// floor in maintenance. Consumers fall back to the record's creation time.
func artifactEventTime(input component.Artifact) time.Time {
	if value := input.Metadata["event_time"]; value != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func artifactAddress(metadata corememory.Metadata) (corememory.Scope, string, bool) {
	scope := corememory.Scope{RuntimeID: metadata["runtime_id"], UserID: metadata["user_id"], AgentID: metadata["agent_id"]}
	conversationID := metadata["conversation_id"]
	return scope, conversationID, scope.Validate() == nil && strings.TrimSpace(conversationID) != ""
}

func digestSources(sources []corememory.SourceRef) string {
	cloned := append([]corememory.SourceRef(nil), sources...)
	sort.Slice(cloned, func(i, j int) bool { return sourceLess(cloned[i], cloned[j]) })
	data, _ := json.Marshal(cloned)
	sum := sha256.Sum256(append([]byte("flowcraft.memory.fact.source\x00"), data...))
	return hex.EncodeToString(sum[:])
}

func sourceLess(left, right corememory.SourceRef) bool {
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.ID != right.ID {
		return left.ID < right.ID
	}
	if left.Revision != right.Revision {
		return left.Revision < right.Revision
	}
	return left.Locator < right.Locator
}

func encodeStrings(values []string) string {
	data, _ := json.Marshal(values)
	return string(data)
}

func decodeStrings(value string) []string {
	var values []string
	if json.Unmarshal([]byte(value), &values) != nil {
		return nil
	}
	return values
}

func overlaps(left, right []string) bool {
	values := make(map[string]struct{}, len(left))
	for _, value := range left {
		values[value] = struct{}{}
	}
	for _, value := range right {
		if _, ok := values[value]; ok {
			return true
		}
	}
	return false
}

func withinFiveMinutes(left, right time.Time) bool {
	if left.IsZero() || right.IsZero() {
		return false
	}
	delta := left.Sub(right)
	if delta < 0 {
		delta = -delta
	}
	return delta <= 5*time.Minute
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func supports(operations []model.Operation, wanted model.Operation) bool {
	return slices.Contains(operations, wanted)
}

func embedItem(text string) inference.EmbedItem {
	return inference.EmbedItem{Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: text}}}}
}

func cosine(left, right []float32) (float64, bool) {
	if len(left) == 0 || len(left) != len(right) {
		return 0, false
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		l, r := float64(left[index]), float64(right[index])
		dot += l * r
		leftNorm += l * l
		rightNorm += r * r
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0, false
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm)), true
}

func cloneMetadata(value corememory.Metadata) corememory.Metadata {
	if value == nil {
		return corememory.Metadata{}
	}
	cloned := make(corememory.Metadata, len(value)+8)
	maps.Copy(cloned, value)
	return cloned
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}
