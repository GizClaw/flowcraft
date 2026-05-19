package compiler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Extractor turns raw input into candidate facts.
//
// PR-4 splits the extractor interface from its implementations.
// Three implementations ship in-tree:
//
//   - passthroughExtractor: returns caller-supplied Facts verbatim.
//     This is the deterministic baseline used when Input.Text is
//     empty or when callers explicitly construct structured facts.
//   - LLMExtractor: routes Input.Text through a pluggable LLM client
//     that returns a JSON document matching ExtractedFactSchema.
//     The implementation uses sdk/llm and tests exercise it with fake
//     clients so no network calls are required.
//   - StaticExtractor: returns a fixed list of facts, regardless of
//     input. Useful in tests that need deterministic non-empty
//     extraction without an LLM round trip.
//
// Implementations must respect Input.Scope: returned facts get
// their scope populated by the compiler core, but extractors
// should not invent scopes of their own.
type Extractor interface {
	Extract(ctx context.Context, input Input) ([]model.TemporalFact, error)
}

type passthroughExtractor struct{}

// Extract returns the caller-supplied facts unchanged. It
// deliberately ignores Input.Text — text-driven extraction is opt
// in via LLMExtractor.
func (passthroughExtractor) Extract(_ context.Context, input Input) ([]model.TemporalFact, error) {
	if len(input.Facts) == 0 {
		return nil, nil
	}
	out := make([]model.TemporalFact, len(input.Facts))
	for i, f := range input.Facts {
		out[i] = f.Clone()
	}
	return out, nil
}

// ExtractedFactSchema is the JSON schema the LLMExtractor enforces
// via llm.WithJSONSchema. It mirrors the v2 TemporalFact shape with
// a closed Kind enum and explicit field types so structured-output
// modes (OpenAI strict mode, Gemini schema, etc.) validate
// server-side.
const ExtractedFactSchema = `{
  "type": "object",
  "properties": {
    "facts": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "kind": {
            "type": "string",
            "enum": ["event", "state", "preference", "relation", "plan", "note"]
          },
          "content": {"type": "string"},
          "subject": {"type": "string"},
          "predicate": {"type": "string"},
          "object": {"type": "string"},
          "entities": {"type": "array", "items": {"type": "string"}},
          "participants": {"type": "array", "items": {"type": "string"}},
          "location": {"type": "string"},
          "valid_from_hint": {"type": "string"},
          "valid_to_hint": {"type": "string"},
          "confidence": {"type": "number"},
          "evidence_text": {"type": "string"},
          "evidence_refs": {
            "type": "array",
            "items": {
              "type": "object",
              "properties": {
                "id": {"type": "string"},
                "message_id": {"type": "string"},
                "role": {"type": "string"},
                "text": {"type": "string"},
                "timestamp": {"type": "string"}
              }
            }
          },
          "source_message_ids": {"type": "array", "items": {"type": "string"}}
        },
        "required": ["kind"]
      }
    }
  },
  "required": ["facts"]
}`

// ExtractedFactList is the wire shape returned by the LLM. Kept
// separate from model.TemporalFact so JSON tags do not leak into
// the canonical model.
type ExtractedFactList struct {
	Facts []ExtractedFact `json:"facts"`
}

// ExtractedFact mirrors model.TemporalFact's caller-relevant
// fields. ValidFrom/ValidTo are passed as relative-time hints that
// the TimeResolver later parses; IDs, MergeKey, Supersedes etc.
// are owned by the compiler / store and never produced by the LLM.
type ExtractedFact struct {
	Kind             string                 `json:"kind"`
	Content          string                 `json:"content,omitempty"`
	Subject          string                 `json:"subject,omitempty"`
	Predicate        string                 `json:"predicate,omitempty"`
	Object           string                 `json:"object,omitempty"`
	Entities         []string               `json:"entities,omitempty"`
	Participants     []string               `json:"participants,omitempty"`
	Location         string                 `json:"location,omitempty"`
	ValidFromHint    string                 `json:"valid_from_hint,omitempty"`
	ValidToHint      string                 `json:"valid_to_hint,omitempty"`
	Confidence       float64                `json:"confidence,omitempty"`
	EvidenceText     string                 `json:"evidence_text,omitempty"`
	EvidenceRefs     []ExtractedEvidenceRef `json:"evidence_refs,omitempty"`
	SourceMessageIDs []string               `json:"source_message_ids,omitempty"`
}

// ExtractedEvidenceRef is the LLM wire shape for evidence pointers.
// The timestamp is intentionally a string so eval adapters can pass
// RFC3339 values when available while leaving benchmark-specific date
// formats to the surrounding raw text.
type ExtractedEvidenceRef struct {
	ID        string `json:"id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Role      string `json:"role,omitempty"`
	Text      string `json:"text,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// LLMExtractorSystemPrompt is the canonical system framing. It is
// intentionally short — product-specific prompt tuning belongs at the
// LLM-client adapter or caller option layer, not inside the compiler.
const LLMExtractorSystemPrompt = `You extract structured memory facts from a conversation snippet.
Return JSON matching the supplied schema. Only emit facts that are
clearly present in the snippet; never fabricate facts to fill the
schema. Use the closed enum for "kind": event | state | preference
| relation | plan | note. If the input marks turns with ids, cite
the supporting turns in evidence_refs and source_message_ids.`

// LLMExtractor calls a sdk/llm.LLM and converts its JSON reply
// into model.TemporalFact values.
//
// The extractor uses the canonical FlowCraft LLM facade directly
// (rather than a recall-local "narrow port") so it inherits
// provider routing, structured-output, caps middleware, fallback,
// and telemetry from the same plumbing every other subsystem uses.
//
// Behaviour:
//   - Empty Input.Text or nil Client falls back to passthrough
//     (callers can prime extraction or skip it).
//   - Input.Facts are returned verbatim alongside any LLM-extracted
//     facts so callers can mix structured + free-form inputs.
//   - The LLM call enforces ExtractedFactSchema via
//     llm.WithJSONSchema; providers that don't natively support
//     structured outputs get the schema through llm/with_caps
//     downgrade automatically.
//   - llm.ExtractJSON tolerates ```json fences and prose around the
//     JSON body so we do not have to engineer around imperfect
//     prompt adherence.
type LLMExtractor struct {
	// Client is the LLM facade. nil disables LLM extraction
	// entirely (extractor degrades to passthrough).
	Client llm.LLM
	// System overrides the default system prompt. Empty falls back
	// to LLMExtractorSystemPrompt.
	System string
	// SchemaName labels the JSON schema in structured-output mode.
	// Defaults to "recall_extracted_facts".
	SchemaName string
	// Temperature is passed via llm.WithTemperature when non-zero.
	Temperature float64
	// ExtraOptions lets callers append provider-specific options
	// (e.g. JSON mode toggles for legacy backends).
	ExtraOptions []llm.GenerateOption
}

// NewLLMExtractor wires an llm.LLM with the default system prompt.
func NewLLMExtractor(client llm.LLM) *LLMExtractor {
	return &LLMExtractor{
		Client:     client,
		System:     LLMExtractorSystemPrompt,
		SchemaName: "recall_extracted_facts",
	}
}

// Extract implements Extractor.
func (e *LLMExtractor) Extract(ctx context.Context, input Input) ([]model.TemporalFact, error) {
	out := make([]model.TemporalFact, 0, len(input.Facts))
	for _, f := range input.Facts {
		out = append(out, f.Clone())
	}
	if strings.TrimSpace(input.Text) == "" || e.Client == nil {
		return out, nil
	}

	system := e.System
	if system == "" {
		system = LLMExtractorSystemPrompt
	}
	schemaName := e.SchemaName
	if schemaName == "" {
		schemaName = "recall_extracted_facts"
	}

	messages := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, system),
		llm.NewTextMessage(llm.RoleUser, input.Text),
	}
	opts := []llm.GenerateOption{
		llm.WithJSONSchema(llm.JSONSchemaParam{
			Name:   schemaName,
			Schema: json.RawMessage(ExtractedFactSchema),
			Strict: true,
		}),
		llm.WithJSONMode(true),
	}
	if e.Temperature != 0 {
		opts = append(opts, llm.WithTemperature(e.Temperature))
	}
	opts = append(opts, e.ExtraOptions...)

	reply, _, err := e.Client.Generate(ctx, messages, opts...)
	if err != nil {
		// %w preserves any errdefs classification the LLM facade
		// already attached (NotAvailable, RateLimit, Timeout, …)
		// so callers still see the original category.
		return nil, fmt.Errorf("recall extractor: llm: %w", err)
	}
	body := reply.Content()
	if body == "" {
		return out, nil
	}
	jsonBytes, _, err := llm.ExtractJSON(body)
	if err != nil {
		// Malformed body is a contract failure on the LLM side
		// but presents to the caller as bad input to the
		// extractor — Validation lets HTTP shims map to 400 and
		// keeps it distinguishable from a transient LLM outage.
		return nil, errdefs.Validation(fmt.Errorf("recall extractor: extract llm json: %w", err))
	}
	var parsed ExtractedFactList
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		return nil, errdefs.Validation(fmt.Errorf("recall extractor: parse llm json: %w", err))
	}
	for _, ef := range parsed.Facts {
		out = append(out, ef.toTemporalFact())
	}
	return out, nil
}

// toTemporalFact converts an ExtractedFact into the canonical
// TemporalFact. ValidFrom/ValidTo hints land in Metadata so the
// TimeResolver picks them up; IDs, merge keys, supersedes pointers
// remain unset for the compiler core to fill.
func (e ExtractedFact) toTemporalFact() model.TemporalFact {
	f := model.TemporalFact{
		Kind:             model.FactKind(e.Kind),
		Content:          e.Content,
		Subject:          e.Subject,
		Predicate:        e.Predicate,
		Object:           e.Object,
		Entities:         append([]string(nil), e.Entities...),
		Participants:     append([]string(nil), e.Participants...),
		Location:         e.Location,
		Confidence:       e.Confidence,
		EvidenceText:     e.EvidenceText,
		EvidenceRefs:     e.toEvidenceRefs(),
		SourceMessageIDs: append([]string(nil), e.SourceMessageIDs...),
	}
	if e.ValidFromHint != "" || e.ValidToHint != "" {
		f.Metadata = map[string]any{}
		if e.ValidFromHint != "" {
			f.Metadata[MetaValidFromHint] = e.ValidFromHint
		}
		if e.ValidToHint != "" {
			f.Metadata[MetaValidToHint] = e.ValidToHint
		}
	}
	return f
}

func (e ExtractedFact) toEvidenceRefs() []model.EvidenceRef {
	if len(e.EvidenceRefs) == 0 {
		return nil
	}
	out := make([]model.EvidenceRef, 0, len(e.EvidenceRefs))
	for _, ref := range e.EvidenceRefs {
		id := strings.TrimSpace(ref.ID)
		messageID := strings.TrimSpace(ref.MessageID)
		text := strings.TrimSpace(ref.Text)
		role := strings.TrimSpace(ref.Role)
		if id == "" && messageID == "" && text == "" {
			continue
		}
		out = append(out, model.EvidenceRef{
			ID:        id,
			MessageID: messageID,
			Role:      role,
			Text:      text,
			Timestamp: parseEvidenceTimestamp(ref.Timestamp),
		})
	}
	return out
}

func parseEvidenceTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t
	}
	return time.Time{}
}

// StaticExtractor returns a fixed list of facts on every call. It
// is the test-friendly counterpart to passthroughExtractor for
// scenarios that need deterministic non-empty extraction without
// involving the LLM interface at all.
type StaticExtractor struct {
	Facts []model.TemporalFact
}

// Extract implements Extractor.
func (s StaticExtractor) Extract(context.Context, Input) ([]model.TemporalFact, error) {
	out := make([]model.TemporalFact, len(s.Facts))
	for i, f := range s.Facts {
		out[i] = f.Clone()
	}
	return out, nil
}
