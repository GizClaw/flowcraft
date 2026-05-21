package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// ExtractedFactSchema is the JSON schema the LLMExtractor enforces
// via llm.WithJSONSchema. It stays intentionally small — three
// fields per memory — so the LLM can dedicate its capacity to
// reading the snippet rather than filling a 14-field grid.
//
// The LLM emits:
//   - text:          a self-contained natural-language sentence.
//   - kind:          one of the six FactKind enum values, picked
//     from the closed list ["event","state",
//     "preference","relation","plan","note"]. This
//     routes the fact through the right downstream
//     projection (timeline / profile / relation /
//     note-retrieval) without forcing the
//     Structurizer to guess from English keywords.
//   - evidence_refs: ids of the supporting turns.
//
// Everything else (Subject/Predicate/Object, Entities, ValidFrom,
// …) is still derived deterministically by the Structurizer from
// the typed per-turn metadata the adapter passes via Input.Turns
// and from the entity-projection snapshot in Input.KnownEntities.
//
// OpenAI structured-output strict mode rejects any object whose
// `properties` set does not equal its `required` set, and requires
// `additionalProperties: false` on every object. We therefore mark
// every listed property as required; "evidence_refs" stays a
// closed two-property object so even strict providers accept it.
const ExtractedFactSchema = `{
  "type": "object",
  "properties": {
    "memories": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "text": {"type": "string"},
          "kind": {
            "type": "string",
            "enum": ["event", "state", "preference", "relation", "plan", "note"]
          },
          "evidence_refs": {
            "type": "array",
            "items": {
              "type": "object",
              "properties": {
                "id": {"type": "string"},
                "text": {"type": "string"}
              },
              "required": ["id", "text"],
              "additionalProperties": false
            }
          }
        },
        "required": ["text", "kind", "evidence_refs"],
        "additionalProperties": false
      }
    }
  },
  "required": ["memories"],
  "additionalProperties": false
}`

// ExtractedFactList is the wire shape returned by the LLM. Kept
// separate from domain.TemporalFact so JSON tags do not leak into
// the canonical domain.
type ExtractedFactList struct {
	Memories []ExtractedMemory `json:"memories"`
}

// ExtractedMemory is the minimal wire shape the LLM emits. It owns
// only three fields:
//   - Text: a single self-contained natural-language sentence that
//     states ONE memory, with absolute dates / speaker names already
//     baked in so the answer LLM can quote it verbatim.
//   - Kind: one of the FactKind enum values. The schema's enum
//     constraint guarantees the model only emits a recognised value;
//     the Structurizer's keyword fallback only runs when this is
//     empty (legacy schema responses).
//   - EvidenceRefs: ids of the supporting turns the adapter
//     announced in Input.Turns.
//
// Everything else (S/P/O, Entities, ValidFrom, …) is filled by the
// Structurizer from the typed per-turn channel.
type ExtractedMemory struct {
	Text         string                 `json:"text"`
	Kind         string                 `json:"kind,omitempty"`
	EvidenceRefs []ExtractedEvidenceRef `json:"evidence_refs,omitempty"`
}

// ExtractedEvidenceRef is the LLM wire shape for evidence pointers.
// Only ID is required; Text is an optional verbatim quote the LLM
// can attach when the supporting turn span is short enough to embed
// directly (matches the legacy contract so existing adapters keep
// working).
type ExtractedEvidenceRef struct {
	ID   string `json:"id"`
	Text string `json:"text,omitempty"`
}

// LLMExtractorSystemPrompt is the canonical framing for the
// extractor LLM.
//
// The LLM writes a self-contained natural-language sentence per
// memory, picks one Kind label from the closed enum, and cites the
// supporting turn ids. Every other field — entities,
// subject/predicate/object, valid_from — is filled by the
// Structurizer stage from the typed per-turn channel the adapter
// already provides (id, time, speaker, role, text). Keeping the
// LLM contract this small keeps smaller models accurate while
// still letting them own the one classification (Kind) that
// keyword tables in the Structurizer cannot do reliably: the SDK
// projections still see canonical TemporalFacts because Kind +
// Structurizer + the typed channel produce them together.
//
// The user message is JSONL — one
// {"id","time","speaker","role","text"} object per line — and the
// LLM must cite the supporting turn(s) by their "id". Callers that
// only have unstructured prose pass a single port.TurnContext with just
// the Text field populated; the SDK then synthesises an id and
// degrades the evidence_refs.id requirement to "best-effort".
const LLMExtractorSystemPrompt = `You extract memories from a conversation snippet.

Output: a JSON object {"memories": [...]} matching the supplied schema.
Each memory has a self-contained sentence, a kind label, and citations
to the supporting turn ids.

The user message contains source turns as JSON, one per line:
{"id":"<turn-id>","time":"<RFC3339 timestamp or empty>","speaker":"<name>","role":"user|assistant","text":"<utterance>"}

Rules:
- One memory per distinct fact. If a turn states "Alice owns a dog
  and lives in Paris", emit TWO memories. Atomic memories rank
  well in retrieval; compound sentences fragment the ranking
  signal.
- "text" MUST be ONE concise English sentence that stands alone,
  so it can be read in isolation by any downstream consumer:
    * use the canonical speaker name when known (the turn's
      "speaker" field, never "user" / "assistant");
    * when the turn carries an absolute timestamp, keep that date
      inline in the sentence so retrieval and rendering see it
      without parsing structured fields (e.g. "On 2030-06-12,
      Alice signed up for the photography class.");
    * spell out the specific entities the turn mentions (people,
      places, organisations, products, identifiers). Concrete
      nouns are what later queries match on.
- "kind" picks ONE label from this closed set:
    * "event"      — something that happened at a specific time
                     ("Alice went to the dentist on 2030-06-12.").
    * "state"      — a durable attribute of a person / entity
                     ("Alice lives in Paris.", "Bob is a chef.",
                     "Carol is 32 years old.").
    * "preference" — a like / dislike / favourite / habit
                     ("Alice loves black coffee.").
    * "relation"   — an interpersonal tie
                     ("Alice is married to Bob.").
    * "plan"       — a stated intention / scheduled future action
                     ("Alice plans to visit Paris next month.").
    * "note"       — anything that does not fit the five above.
                     Default to "note" if uncertain; never invent
                     a label outside the list.
- "evidence_refs" lists the turn id(s) that support the memory.
  Cite every supporting turn AT MOST ONCE. ID values must match
  one of the "id"s in the input verbatim — never invent ids,
  never paraphrase.
- "evidence_refs[].text" (optional) is a short verbatim quote
  from the supporting turn (≤ 200 chars). Keep the wording faithful
  to the original turn; never paraphrase.
- Only emit memories that are clearly present in the snippet; never
  fabricate to fill the schema. Returning {"memories": []} is the
  right answer when the snippet says nothing memorable.`

// LLMExtractor calls a sdk/llm.LLM and converts its JSON reply
// into domain.TemporalFact values.
//
// The extractor uses the canonical FlowCraft LLM facade directly
// (rather than a recall-local "narrow port") so it inherits
// provider routing, structured-output, caps middleware, fallback,
// and telemetry from the same plumbing every other subsystem uses.
//
// Behaviour:
//   - Empty Input.Turns or nil Client falls back to passthrough
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

var _ port.Extractor = (*LLMExtractor)(nil)

// NewLLMExtractor wires an llm.LLM with the default system prompt.
func NewLLMExtractor(client llm.LLM) *LLMExtractor {
	return &LLMExtractor{
		Client:     client,
		System:     LLMExtractorSystemPrompt,
		SchemaName: "recall_extracted_facts",
	}
}

// Extract implements Extractor.
//
// Path:
//  1. Caller-supplied Input.Facts pass through unchanged (clone).
//  2. If Input.Turns is non-empty, render to JSONL and call the LLM
//     with the 3-field memories schema (text + kind + evidence_refs).
//     Each parsed memory becomes a TemporalFact with Content + Kind +
//     EvidenceRefs populated; Structurizer fills the rest downstream
//     (and only falls back to keyword-based Kind inference when the
//     LLM left Kind empty, e.g. legacy schema responses).
//  3. Empty Turns / nil client → no-op (passthrough only). For
//     unstructured prose callers pass a single port.TurnContext with
//     only Text populated — there is no separate Text channel.
func (e *LLMExtractor) Extract(ctx context.Context, input port.IngestInput) ([]domain.TemporalFact, error) {
	out := make([]domain.TemporalFact, 0, len(input.Facts))
	for _, f := range input.Facts {
		out = append(out, f.Clone())
	}

	userMessage, turnIndex, ok := buildExtractorUserMessage(input)
	if !ok || e.Client == nil {
		return out, nil
	}
	if prefix := formatExtractorContextPrefix(input); prefix != "" {
		userMessage = prefix + userMessage
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
		llm.NewTextMessage(llm.RoleUser, userMessage),
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
		return nil, fmt.Errorf("recall extractor: llm: %w", err)
	}
	body := reply.Content()
	if body == "" {
		return out, nil
	}
	jsonBytes, _, err := llm.ExtractJSON(body)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("recall extractor: extract llm json: %w", err))
	}
	parsed, err := parseExtractorReply(jsonBytes)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf("recall extractor: parse llm json: %w", err))
	}
	for _, m := range parsed.Memories {
		text := strings.TrimSpace(m.Text)
		if text == "" {
			continue
		}
		fact := domain.TemporalFact{
			Content:      text,
			EvidenceText: text,
			Kind:         normaliseExtractedKind(m.Kind),
			EvidenceRefs: extractedEvidenceRefs(m.EvidenceRefs, turnIndex),
		}
		fact.SourceMessageIDs = sourceIDsFromEvidence(fact.EvidenceRefs)
		out = append(out, fact)
	}
	return out, nil
}

// normaliseExtractedKind maps the LLM's "kind" field to a canonical
// FactKind. Empty / unrecognised values fall through to KindNote-
// equivalent (empty string) so the Structurizer's keyword fallback
// stays in charge of classification when the model could not pick.
// Lowercasing covers minor casing drift from less-strict providers.
func normaliseExtractedKind(raw string) domain.FactKind {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "event":
		return domain.KindEvent
	case "state":
		return domain.KindState
	case "preference":
		return domain.KindPreference
	case "relation":
		return domain.KindRelation
	case "plan":
		return domain.KindPlan
	case "note":
		return domain.KindNote
	}
	return ""
}

// parseExtractorReply accepts either the new {"memories": [...]} shape
// or the legacy {"facts": [...]} shape so a deployment can roll the
// prompt slim-down forward without flushing the LLM client cache. The
// legacy parser only reads "content" + "evidence_refs.id" so all the
// old structural fields are silently dropped — Structurizer fills
// them downstream regardless of which schema the model returned.
func parseExtractorReply(body []byte) (ExtractedFactList, error) {
	var parsed ExtractedFactList
	if err := json.Unmarshal(body, &parsed); err == nil && len(parsed.Memories) > 0 {
		return parsed, nil
	}
	type legacyEvidence struct {
		ID        string `json:"id"`
		MessageID string `json:"message_id"`
		Text      string `json:"text"`
	}
	type legacyFact struct {
		Content      string           `json:"content"`
		Text         string           `json:"text"`
		Kind         string           `json:"kind"`
		EvidenceRefs []legacyEvidence `json:"evidence_refs"`
	}
	var legacy struct {
		Facts    []legacyFact `json:"facts"`
		Memories []legacyFact `json:"memories"`
	}
	if err := json.Unmarshal(body, &legacy); err != nil {
		return ExtractedFactList{}, err
	}
	merged := legacy.Facts
	if len(merged) == 0 {
		merged = legacy.Memories
	}
	for _, lf := range merged {
		text := strings.TrimSpace(lf.Text)
		if text == "" {
			text = strings.TrimSpace(lf.Content)
		}
		mem := ExtractedMemory{Text: text, Kind: lf.Kind}
		for _, ref := range lf.EvidenceRefs {
			id := strings.TrimSpace(ref.ID)
			if id == "" {
				id = strings.TrimSpace(ref.MessageID)
			}
			if id == "" {
				continue
			}
			mem.EvidenceRefs = append(mem.EvidenceRefs, ExtractedEvidenceRef{ID: id, Text: ref.Text})
		}
		parsed.Memories = append(parsed.Memories, mem)
	}
	return parsed, nil
}

// extractedEvidenceRefs converts the LLM-side evidence list into
// canonical EvidenceRefs and enriches each ref with the typed
// per-turn metadata (Role, Timestamp) from the request-time turn
// index. Duplicate refs are collapsed: when the LLM cites the same
// turn under multiple slight variations (same id but different text
// quote), the duplicates inflate the projection's BM25 document
// without adding signal. We keep the first occurrence per
// (id || normalized text) key.
func extractedEvidenceRefs(refs []ExtractedEvidenceRef, turnIndex map[string]port.TurnContext) []domain.EvidenceRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]domain.EvidenceRef, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		id := strings.TrimSpace(ref.ID)
		text := strings.TrimSpace(ref.Text)
		if id == "" && text == "" {
			continue
		}
		key := evidenceRefDedupeKey(id, "", text)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		evidence := domain.EvidenceRef{
			ID:        id,
			MessageID: id,
			Text:      text,
		}
		if turn, ok := turnIndex[id]; ok {
			evidence.Role = turn.Role
			if !turn.Time.IsZero() {
				evidence.Timestamp = turn.Time
			}
			if evidence.Text == "" {
				evidence.Text = turn.Text
			}
		}
		out = append(out, evidence)
	}
	return out
}

// sourceIDsFromEvidence projects evidence ids into the
// SourceMessageIDs slice so legacy callers reading SourceMessageIDs
// still see the same ids. Order matches evidence order; duplicates
// are already deduped by extractedEvidenceRefs.
func sourceIDsFromEvidence(refs []domain.EvidenceRef) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.ID == "" {
			continue
		}
		out = append(out, r.ID)
	}
	return out
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
