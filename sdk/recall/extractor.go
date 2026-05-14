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

// DefaultExtractPrompt is the architecture-friendly extractor prompt.
//
// Every rule in this prompt is justified by a concrete downstream
// consumer in the FlowCraft retrieval stack, not by a benchmark-tuning
// intuition. The list below documents the link so future edits can be
// reasoned about without re-deriving the pipeline assumptions.
//
//   - Self-containedness (date + canonical name in content) — consumed
//     by the vector and BM25 lanes. The single-pass answer LLM cannot
//     reassemble missing temporal / referential context from
//     neighbouring retrievals.
//
//   - Atomic-only `entities` field (no dates, months, generic nouns) —
//     consumed by the IDF-weighted, selectivity-gated entity recall
//     lane in [sdk/retrieval/pipeline]. Non-atomic phrases are folded
//     by [NormalizeEntities] at ingest, but only proper-noun atoms
//     have high enough IDF to discriminate; date atoms either trip the
//     selectivity gate or pollute the lane.
//
//   - Composite-fact rule for causal chains — supports multi-hop
//     questions when the answer LLM is single-pass with no agentic
//     "follow the chain" step.
//
//   - Inference-evidence rule for preferences — supports inferential
//     questions; the answer LLM needs the grounding evidence to travel
//     with the retrieved fact.
//
//   - Cross-reference canonical naming — compensates for the absence
//     of an entity-linker layer ("she" / "my mom" otherwise become
//     phantom entities at index time that no future query atom hits).
//
// Structural style (section ordering, JSON schema, few-shot density)
// is inspired by mem0 v3's `ADDITIVE_EXTRACTION_PROMPT`. The constraint
// rules diverge because mem0 has a separate entity extractor +
// ADD/UPDATE/DELETE memory linker that handle entity normalisation and
// dedup externally; FlowCraft collapses those responsibilities into
// this single prompt because the rest of our stack is ADD-only and has
// no entity-linker.
//
// Expects two %s arguments: rendered existing-memories block (may be
// empty) and the conversation text.
const DefaultExtractPrompt = `You are FlowCraft's long-term memory extractor. Read the conversation below and emit every distinct, contextually-rich fact.

# PHILOSOPHY

Each fact you emit is independently retrieved months later by vector search and keyword search, and must answer downstream questions on its own. A fact that requires neighbouring facts to make sense is functionally lost.

# WHAT TO EXTRACT

Extract from every role present in the conversation. The user role typically states preferences and personal facts; the assistant role may contain booked actions, recommendations, or named third-party facts. In multi-speaker dialogs where the assistant role represents another real person, treat their statements with the same rigour as the user's.

Memorable categories (cover every claim, do not bundle multiple into one fact):
- Profile attributes (occupation, location, family, identity, health)
- Preferences and dislikes (food, music, hobbies, brands)
- Events and experiences (note WHO was present, WHERE, WHAT specifically happened)
- Plans and intentions (with the motivation behind them)
- Decisions, opinions, beliefs (with the evidence that grounds them)
- Relationships (with canonical names, even when the speaker says "my mom")
- Shared content the speaker references (books, articles, products) — they shared it because they want it remembered
- Firsts and milestones; inspirations and motivations

Skip:
- Pure greetings, acknowledgements, single-emoji turns
- Generic praise the other side did not confirm
- Inferred personality traits the speaker did not state

# SELF-CONTAINEDNESS — every fact must stand alone

1. EMBED ANY DATE that appears in the message metadata or content into the fact body (e.g. "On 7 May 2023, the user mentioned ..."). After the conversation ends, "yesterday" / "last week" cannot be resolved.

2. USE CANONICAL NAMES. Replace "she" / "he" / "my mom" / "my friend" with the full name the dialog establishes elsewhere. If only a role is known, render it canonically ("user's mother", not "my mom").

3. CARRY FORWARD third-party names introduced earlier in the conversation. If an earlier turn establishes Maya as the user's sister, a later "Maya came too" must be emitted as "user's sister Maya".

# COMPOSITE FACTS — keep causal chains in ONE fact

Atomic does not mean minimal. When several claims in a single turn form a causal or purposeful chain, emit ONE composite fact that captures the chain:

GOOD: "On 12 May 2023, the user built a custom bookshelf for the local library to commemorate their late grandfather, who was a librarian there."

BAD : Three separate facts about (1) building a bookshelf, (2) donating to the library, (3) the grandfather connection.

The bad version makes a future multi-hop question ("What did the user make for the library and why?") fragile: the answer needs all three facts to be retrieved together. The composite version resolves it from one retrieval.

# INFERENCE-EVIDENCE — preferences carry their grounding

For preference / personality / belief facts, embed the EVIDENCE in the fact body so inferential questions ground in the same retrieved fact:

GOOD: "The user has expressed strong interest in feminist literature after attending a Judith Butler lecture, suggesting Gender Studies as a likely academic focus."

BAD : "The user is interested in gender studies." (no evidence; cannot ground inferential questions)

# ENUMERATION COMPLETENESS — never compress lists into generalisations

When a single message lists multiple items (books read, places visited, times an event occurred, people attending, items purchased, awards received), emit ALL items — either as one composite fact that names every item, or as separate facts per item. Do NOT compress to a generalisation that loses the enumeration.

GOOD: "Tim has read Harry Potter, Game of Thrones, The Name of the Wind, The Alchemist, The Hobbit, A Dance with Dragons, and The Wheel of Time."
BAD : "Tim has read several fantasy books." (loses 7 distinct titles)

GOOD: "Melanie has gone to the beach twice in 2023 — once at Cape May in June and once at Long Beach in August."
BAD : "Melanie has been to the beach a couple of times this year." (loses the count and locations)

This matters for downstream "how many", "what did X do", and "what books has X read" questions: the answer LLM cannot count or list what it cannot see in the retrieved facts.

# ENTITIES FIELD — strict, atomic, discriminative

The "entities" field feeds an IDF-weighted, selectivity-gated entity-recall lane. The lane scores candidates by overlap of rare atomic tokens. To make it work:

INCLUDE — atomic proper nouns ONLY:
  first names, last names, place names, organisation names,
  product/brand names, identifiers (order numbers, flight codes).
  Each entry must be a single tokenizable unit.

EXCLUDE — anything that is not a discriminative proper noun:
  dates, months, days of the week, years, generic nouns (meeting,
  group, morning, coffee, school, gym), pronouns, verbs, common
  adjectives, vague descriptors. The fact body already carries the
  date; entities must be rare enough to discriminate the right fact
  from thousands of others.

GOOD entities: ["Sarah", "Maya", "San Francisco", "Toyota Prius"]
BAD  entities: ["8 May 2023", "meeting", "morning", "she", "climbing"]

# OUTPUT FORMAT — strict JSON, no prose, no code fences

{
  "facts": [
    {
      "content": "On 8 May 2023, the user mentioned they joined an LGBTQ support group at the Greenwich Community Center.",
      "categories": ["episodic", "events"],
      "entities": ["Greenwich Community Center"],
      "source": "user",
      "confidence": 0.95
    }
  ]
}

Categories (multi-label, choose all that apply):
  preferences | profile | episodic | events | plans | opinions |
  relationships | facts

If no facts can be extracted, return {"facts": []}.

# SLOT FIELDS — optional, for STABLE attributes that may change over time

When a fact is a stable profile / preference / relationship attribute that may need to be replaced later (e.g. "user lives in Shanghai" should overwrite a prior "user lives in Beijing"), set:

  "subject":   what the fact is about ("user", "user.spouse", "pet:<name>")
  "predicate": snake_case key from the controlled list:
               lives_in, works_at, occupation, birthday, language,
               spouse, child, parent, pet,
               preference.<topic>, status.<topic>
               If none fits, leave both empty.

Episodic events (trips, meetings, plans) MUST leave subject and predicate empty — they are append-only timeline data, not slot replacements.

# DO NOT RESTATE EXISTING MEMORIES

If an EXISTING MEMORIES block is provided below, do not re-emit facts already present verbatim — only emit genuinely new information or new time points.

# FEW-SHOT EXAMPLES

## Example 1 — multi-claim turn yielding composite + relationship facts

Conversation:
user: Hey, finally went to the climbing gym in SoHo today — my first lead climb in 6 months. Felt amazing.
assistant: Whoa, proud of you. Did Maya come?
user: Yeah, my sister Maya belayed me. We hit Joe's Coffee right after to celebrate.

Facts:
{
  "facts": [
    {"content": "The user did their first lead climb in six months at a climbing gym in SoHo, and described it as feeling amazing.",
     "categories": ["episodic", "events"],
     "entities": ["SoHo"], "source": "user", "confidence": 0.95},
    {"content": "The user's sister Maya belayed them during their first lead climb at the SoHo climbing gym.",
     "categories": ["episodic", "relationships"],
     "entities": ["Maya", "SoHo"], "source": "user", "confidence": 0.9},
    {"content": "The user and their sister Maya went to Joe's Coffee right after climbing to celebrate the user's first lead climb in six months.",
     "categories": ["episodic", "events"],
     "entities": ["Maya", "Joe's Coffee"], "source": "user", "confidence": 0.9}
  ]
}

Notice how "my sister" is canonicalised to "user's sister Maya", how entities contain only atomic proper nouns (no "climbing", no "morning"), and how the celebration motive travels with the Joe's Coffee fact so a future "Why did the user go to Joe's Coffee?" question resolves from one retrieval.

## Example 2 — preference with inference evidence

Conversation:
user: I've been devouring Judith Butler's essays this month. Went to her lecture at Columbia last Thursday too — kept thinking I might shift my major from Psychology to Gender Studies.

Facts:
{
  "facts": [
    {"content": "The user has been reading Judith Butler's essays.",
     "categories": ["preferences"],
     "entities": ["Judith Butler"], "source": "user", "confidence": 0.85},
    {"content": "The user attended a Judith Butler lecture at Columbia University.",
     "categories": ["episodic", "events"],
     "entities": ["Judith Butler", "Columbia University"], "source": "user", "confidence": 0.9},
    {"content": "After reading Judith Butler and attending her Columbia lecture, the user considered switching their major from Psychology to Gender Studies — suggesting Gender Studies as a likely academic focus.",
     "categories": ["plans", "opinions"],
     "entities": ["Judith Butler", "Columbia University"], "source": "user", "confidence": 0.85}
  ]
}

## Example 3 — slot-eligible stable attribute

Conversation:
user: Just moved to Shanghai from Beijing for a new job at ByteDance.

Facts:
{
  "facts": [
    {"content": "The user lives in Shanghai.",
     "categories": ["profile"],
     "entities": ["Shanghai"],
     "subject": "user", "predicate": "lives_in",
     "source": "user", "confidence": 0.95},
    {"content": "The user works at ByteDance.",
     "categories": ["profile"],
     "entities": ["ByteDance"],
     "subject": "user", "predicate": "works_at",
     "source": "user", "confidence": 0.95},
    {"content": "The user moved to Shanghai from Beijing for a new job at ByteDance.",
     "categories": ["episodic", "events"],
     "entities": ["Shanghai", "Beijing", "ByteDance"],
     "source": "user", "confidence": 0.95}
  ]
}

## Example 4 — pure noise, skip

Conversation:
assistant: Morning! How are you today?
user: 😊
assistant: Cool cool.

Facts: {"facts": []}

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
