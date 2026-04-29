package recall

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// ExtractMode selects the extraction strategy.
type ExtractMode string

const (
	// ModeAdditive is the default: one LLM call, ADD-only, no merge/delete.
	ModeAdditive ExtractMode = "additive"
	// ModeReconciling is reserved for the legacy two-pass merge/delete behavior.
	// As of Phase 3, no implementation ships in tree.
	ModeReconciling ExtractMode = "reconciling"
)

// Source labels added by for fact provenance.
const (
	SourceUser      = "user"
	SourceAssistant = "assistant"
	SourceSystem    = "system"
	SourceRaw       = "raw"
)

// ExtractedFact is one LLM-extracted memory candidate.
//
// Subject and Predicate are optional slot fields; when BOTH are non-empty
// (and neither contains the slot delimiter '|') the Save path
// activates the deterministic "slot supersede" channel (see
// merger.supersedeBySlot) which marks any older entry sharing the
// same (Subject, Predicate) tuple as superseded_by the new entry.
// Leave them empty for episodic / non-slot facts; behaviour is
// unchanged.
//
// The recommended v1 predicate vocabulary is enumerated in
// [DefaultPredicates]; locale variants and common synonyms are mapped
// to canonical entries by [PredicateAliases] (extend per-namespace
// via [WithPredicateAlias]).
type ExtractedFact struct {
	Content    string   `json:"content"`
	Categories []string `json:"categories,omitempty"`
	Entities   []string `json:"entities,omitempty"`
	Source     string   `json:"source,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`

	Subject   string `json:"subject,omitempty"`
	Predicate string `json:"predicate,omitempty"`
}

// DefaultPredicates is the v1 controlled predicate vocabulary rendered
// into DefaultExtractPrompt. Predicates outside this list are still
// honoured by the slot supersede channel (matching is exact-string), but
// extending the prompt vocabulary should go through this slice so the
// guidance stays consistent.
var DefaultPredicates = []string{
	"lives_in", "works_at", "occupation", "birthday", "language",
	"spouse", "child", "parent", "pet",
	"preference.<topic>", "status.<topic>",
}

// ExtractOptions carries optional context handed to an Extractor invocation.
//
// Fields are populated by the Save path immediately before calling Extract.
// Adding new optional fields is API-additive: existing extractors ignore them.
type ExtractOptions struct {
	// ExistingFacts are short-form summaries of the top-K most relevant
	// memories already in store, intended to be injected into the prompt so
	// the model can avoid restating them. Empty when SaveWithContext is off.
	ExistingFacts []string
}

// ExtractOption mutates ExtractOptions.
type ExtractOption func(*ExtractOptions)

// WithExistingFacts injects existing memory snippets to dampen duplication.
func WithExistingFacts(facts []string) ExtractOption {
	return func(o *ExtractOptions) { o.ExistingFacts = facts }
}

// Extractor produces facts from a list of conversational messages.
//
// Variadic options preserve forward-compat: callers can layer in new
// per-invocation context (existing facts, scope-scoped instructions, …)
// without breaking implementations that don't read them.
type Extractor interface {
	Extract(ctx context.Context, scope Scope, msgs []llm.Message, opts ...ExtractOption) ([]ExtractedFact, error)
}

// applyExtractOptions reduces a slice of ExtractOption into ExtractOptions.
func applyExtractOptions(opts []ExtractOption) ExtractOptions {
	var o ExtractOptions
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	return o
}

// DefaultExtractPrompt is the prompt template (CN+EN bilingual core).
//
// It expects two %s arguments in order: existing-memories block (already
// rendered, may be empty) and the conversation text.
const DefaultExtractPrompt = `You are Flowcraft's long-term memory extractor. Read the following conversation and extract self-contained facts.

HARD CONSTRAINTS
1. Do NOT deduplicate; do NOT merge or overwrite history. Different time points of the same kind of fact must be emitted separately.
2. Skip greetings, confirmations and follow-ups that carry no new information.
3. Paraphrase facts as third-person objective statements ("user prefers black coffee"), not raw dialog.
4. Extract from BOTH user and assistant turns when the assistant message carries a fact ("I booked you a flight on Mar 3" is a fact).
5. If the EXISTING MEMORIES block is provided, do not restate facts that are already present verbatim — only emit genuinely new information or new time points.

OUTPUT FORMAT — strict JSON object with a single "facts" array, no prose:
{
  "facts": [
    {
      "content": "user moved from New York to San Francisco",
      "categories": ["episodic", "profile"],
      "entities": ["New York", "San Francisco"],
      "source": "user",
      "confidence": 0.95
    }
  ]
}

Categories (multi-label allowed): preferences | profile | episodic | procedural | semantic | events | facts | patterns | cases | entities
Entities: people, places, organizations, products, quoted strings, compound nouns. Keep original surface form.

SLOT FIELDS (optional but encouraged for STABLE attributes that can change
over time — profile, preferences, relationships):
  "subject":   what the fact is about ("user", "user.spouse", "pet:<name>")
  "predicate": snake_case key from the controlled list:
               lives_in, works_at, occupation, birthday, language,
               spouse, child, parent, pet,
               preference.<topic>, status.<topic>
               If NONE fits, leave both as empty strings.

Episodic events ("I went to Tokyo last week", "booked a flight on Mar 3")
MUST leave subject and predicate empty — they are append-only timeline
data, not slot replacements.

Slot examples:
  "I moved to Shanghai" →
    {"content":"user lives in Shanghai","subject":"user",
     "predicate":"lives_in","entities":["Shanghai"], ...}
  "I love lattes" →
    {"content":"user prefers lattes","subject":"user",
     "predicate":"preference.coffee","entities":["latte"], ...}

If no facts: return {"facts": []}.

%sCONVERSATION:
%s
`

// renderExistingFacts formats existing memories as a "EXISTING MEMORIES" prefix
// suitable for direct insertion into DefaultExtractPrompt's first %s. Returns
// the empty string when facts is empty so the prompt collapses cleanly.
func renderExistingFacts(facts []string) string {
	if len(facts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("EXISTING MEMORIES (do not restate verbatim):\n")
	for _, f := range facts {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(f)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}

// AdditiveExtractor is the default single-pass extractor.
//
// When LLM is nil, Extract returns an empty slice; callers should treat
// Add as the only ingestion path — useful for tests and bench runners
// that need to populate LTM verbatim without spinning up an LLM client.
type AdditiveExtractor struct {
	LLM              llm.LLM
	PromptTemplate   string // %s receives the rendered conversation; defaults to DefaultExtractPrompt
	IncludeAssistant bool   //; default true at New() level
	MaxFacts         int    // truncate results; default 20
	ConfidenceMin    float64
}

// Name returns the extractor name (used for telemetry attributes).
func (e *AdditiveExtractor) Name() string { return "additive" }

// Extract implements Extractor.
func (e *AdditiveExtractor) Extract(ctx context.Context, _ Scope, msgs []llm.Message, opts ...ExtractOption) ([]ExtractedFact, error) {
	if e.LLM == nil || len(msgs) == 0 {
		return nil, nil
	}
	convo := renderConversation(msgs, e.IncludeAssistant)
	if strings.TrimSpace(convo) == "" {
		return nil, nil
	}
	o := applyExtractOptions(opts)
	tmpl := e.PromptTemplate
	if tmpl == "" {
		tmpl = DefaultExtractPrompt
	}
	prompt := fmt.Sprintf(tmpl, renderExistingFacts(o.ExistingFacts), convo)

	resp, _, err := e.LLM.Generate(ctx, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: prompt}}},
	},
		llm.WithJSONSchema(extractedFactsSchema),
		llm.WithJSONMode(true),
	)
	if err != nil {
		return nil, err
	}
	raw := resp.Content()
	out, err := parseFactsJSON(raw)
	if err != nil {
		return nil, err
	}
	if e.ConfidenceMin > 0 {
		out = filterConfidence(out, e.ConfidenceMin)
	}
	if e.MaxFacts > 0 && len(out) > e.MaxFacts {
		out = out[:e.MaxFacts]
	}
	return out, nil
}

func renderConversation(msgs []llm.Message, includeAssistant bool) string {
	var b strings.Builder
	for _, m := range msgs {
		role := string(m.Role)
		if !includeAssistant && (m.Role == model.RoleAssistant || m.Role == model.RoleTool) {
			continue
		}
		txt := strings.TrimSpace(m.Content())
		if txt == "" {
			continue
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(txt)
		b.WriteByte('\n')
	}
	return b.String()
}

// parseFactsJSON parses an LLM-emitted facts response. Envelope detection is
// delegated to [llm.ExtractJSON] (fence-stripping, prose-tolerance, structured-
// over-scalar preference); we only need to pick the right business shape:
//
//  1. {"facts":[…]}    — schema-enforced path (extractedFactsSchema)
//  2. […]               — legacy prompt / providers that ignored the schema
//  3. {"content":…}     — single fact emitted without the wrapper
//
// "facts": null and "facts": [] both decode cleanly to nil — that's a legal
// "nothing to extract" answer, not an error.
func parseFactsJSON(raw string) ([]ExtractedFact, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	payload, _, err := llm.ExtractJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("ltm: extractor: %w", err)
	}

	var env struct {
		Facts *[]ExtractedFact `json:"facts"`
	}
	if err := json.Unmarshal(payload, &env); err == nil && env.Facts != nil {
		return normalizeFacts(*env.Facts), nil
	}

	var bare []ExtractedFact
	if err := json.Unmarshal(payload, &bare); err == nil {
		return normalizeFacts(bare), nil
	}

	var single ExtractedFact
	if err := json.Unmarshal(payload, &single); err == nil && strings.TrimSpace(single.Content) != "" {
		return normalizeFacts([]ExtractedFact{single}), nil
	}

	return nil, errdefs.Validationf("ltm: extractor: cannot parse facts JSON: %s", truncate(payload, 200))
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

func normalizeFacts(in []ExtractedFact) []ExtractedFact {
	out := in[:0]
	for _, f := range in {
		f.Content = strings.TrimSpace(f.Content)
		if f.Content == "" {
			continue
		}
		f.Categories = normStrings(f.Categories)
		f.Entities = normStrings(f.Entities)
		f.Subject = strings.TrimSpace(f.Subject)
		f.Predicate = strings.TrimSpace(f.Predicate)
		out = append(out, f)
	}
	return out
}

func normStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:0]
	seen := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out
}

func filterConfidence(in []ExtractedFact, min float64) []ExtractedFact {
	out := in[:0]
	for _, f := range in {
		if f.Confidence > 0 && f.Confidence < min {
			continue
		}
		out = append(out, f)
	}
	return out
}

// JSONNow is exposed to allow extractor tests to deterministically stamp
// CreatedAt; the regular code path uses time.Now().
var JSONNow = time.Now

// extractedFactsSchema is the OpenAI/Qwen "json_schema" payload sent alongside
// the extractor prompt. Wrapped in an object because most providers reject
// top-level arrays for structured outputs; parseFactsJSON unwraps the {facts}
// envelope before falling back to the legacy bare-array forms.
var extractedFactsSchema = llm.JSONSchemaParam{
	Name:        "extracted_facts",
	Description: "Long-term memory facts extracted from a conversation",
	Strict:      true,
	Schema: map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"facts"},
		"properties": map[string]any{
			"facts": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					// Azure's strict json_schema enforces the OpenAI rule that
					// every key in `properties` must also appear in `required`.
					// Models can still emit empty arrays / "" / 0.0 to signal
					// "no value" — parseFactsJSON tolerates them.
					"required": []string{"content", "categories", "entities", "source", "confidence", "subject", "predicate"},
					"properties": map[string]any{
						"content":    map[string]any{"type": "string"},
						"categories": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"entities":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"source":     map[string]any{"type": "string"},
						"confidence": map[string]any{"type": "number"},
						"subject":    map[string]any{"type": "string"},
						"predicate":  map[string]any{"type": "string"},
					},
				},
			},
		},
	},
}
