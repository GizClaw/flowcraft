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

	// Episodic flags this fact as an append-only timeline record (a
	// dated event, a trip, a meeting, a one-off interaction). It is
	// a first-class architectural signal, NOT a category label: the
	// vector supersede channel in [sdk/recall.supersedeNeighbours]
	// refuses to mark older neighbours as superseded when the new
	// fact is Episodic, so two events sharing the same actors and
	// places but different timestamps never collide on entity-set
	// equality.
	//
	// DefaultExtractPrompt instructs the LLM to emit this field
	// directly. For backward compatibility with extractors that only
	// emit categories, [normalizeFacts] also infers Episodic=true
	// when Categories contains the legacy strings "episodic" or
	// "events".
	Episodic bool `json:"episodic,omitempty"`
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
// The prompt is intentionally dense in two ways the rest of the stack
// relies on: the section ordering puts the highest-leverage rules
// (multi-speaker awareness, temporal grounding, episodic flag) at
// the top so smaller models give them attention budget; and every
// few-shot demonstrates a specific failure mode the JSON post-parser
// or the supersede channels react to.
//
// Expects two %s arguments: rendered existing-memories block (may be
// empty) and the conversation text.
const DefaultExtractPrompt = `You are FlowCraft's long-term memory extractor. Read the conversation below and emit every distinct, contextually-rich fact.

# PHILOSOPHY

Each fact you emit is independently retrieved months later by vector search and keyword search, and must answer downstream questions on its own. A fact that requires neighbouring facts to make sense is functionally lost.

# MULTI-SPEAKER — both wire-protocol roles can be real people

The "user" / "assistant" role labels are wire-protocol artefacts, not a guide to whose facts matter. Treat both roles as equal sources of personal facts unless the assistant is plainly an AI agent (generic acknowledgements, model meta-commentary, recommendation lists with no first-person claims about its own life).

Indicators that the assistant role is a real person:
- It uses first-person ("I went to the gym today")
- It carries a named-speaker hint like "[<datetime>] <Name>: ..." or "<Name>: ..."
- It references its own family / job / location / preferences
- It reciprocates personal anecdotes the user shares

When the assistant role is a real person, attribute every fact BY NAME (not "the assistant"). Failing to extract from the second speaker drops roughly half the facts in any two-party log and makes any question about them unanswerable.

GOOD: "Alice ran her first half-marathon on 14 March 2024."
BAD : "The user ran her first half-marathon on 14 March 2024." (when the conversation establishes Alice as the named speaker)
BAD : (skipping the fact entirely because it was uttered by the assistant role)

# TEMPORAL GROUNDING — anchor every fact to an absolute date

Message turns may carry an observation timestamp in metadata or as a content prefix. A common chat-archive convention is "[<datetime>] <speaker>:" but any timestamp the host application surfaces works the same way. That timestamp is your authoritative observation anchor for everything that turn says — use it to resolve relative references INSIDE the turn:

- "yesterday" → day before the anchor datetime
- "last week" → week preceding the anchor datetime
- "two weekends ago" → ground to a specific calendar weekend
- "recently" / "just" / "today" → on or near the anchor datetime
- "in two months" → calendar month + 2 from the anchor datetime

EVERY fact body must carry an absolute date or duration the answer LLM can read months later. A fact that contains only a relative reference is functionally lost — there is no second pass that can resolve "last week" back to an absolute date.

GOOD: "On 14 March 2024, Alice ran her first half-marathon and finished in 2h 14m."
GOOD: "Alice moved to her current city in early March 2024 (around two weeks before the conversation of 18 March 2024)." (qualifier preserved, anchor explicit)
BAD : "Alice recently ran a half-marathon." (no anchor; useless after the conversation ends)
BAD : "Alice moved last month." (relative; unresolvable)

If the conversation never provides any datetime anywhere, omit the date qualifier rather than fabricating one — but flag such facts as low confidence.

# WHAT TO EXTRACT

Memorable categories (cover every claim by every speaker, do not bundle multiple into one fact):
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

1. USE CANONICAL NAMES. Replace "she" / "he" / "my mom" / "my friend" with the full name the dialog establishes elsewhere. If only a role is known, render it canonically ("user's mother", not "my mom").

2. CARRY FORWARD third-party names introduced earlier in the conversation. If an earlier turn establishes Maya as the user's sister, a later "Maya came too" must be emitted as "user's sister Maya".

# COMPOSITE FACTS — keep causal chains in ONE fact

Atomic does not mean minimal. When several claims in a single turn form a causal or purposeful chain, emit ONE composite fact that captures the chain:

GOOD: "On 12 May 2023, the user built a custom bookshelf for the local library to commemorate their late grandfather, who was a librarian there."

BAD : Three separate facts about (1) building a bookshelf, (2) donating to the library, (3) the grandfather connection.

The bad version makes a future multi-hop question ("What did the user make for the library and why?") fragile: the answer needs all three facts to be retrieved together. The composite version resolves it from one retrieval.

# INFERENCE-EVIDENCE — preferences carry their grounding

For preference / personality / belief facts, embed the EVIDENCE in the fact body so inferential questions ground in the same retrieved fact:

GOOD: "The user has expressed strong interest in feminist literature after attending a Judith Butler lecture, suggesting Gender Studies as a likely academic focus."

BAD : "The user is interested in gender studies." (no evidence; cannot ground inferential questions)

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
      "episodic": true,
      "source": "user",
      "confidence": 0.95
    }
  ]
}

Categories (multi-label, choose all that apply):
  preferences | profile | episodic | events | plans | opinions |
  relationships | facts

If no facts can be extracted, return {"facts": []}.

# EPISODIC FLAG — first-class architectural signal

Set "episodic": true when the fact records a dated event, trip, meeting, encounter, or any one-off interaction with a specific timestamp. Two different events sharing the same actors / places but different dates ("Caroline went to the support group on 7 May" vs "on 8 May") MUST both be marked episodic so the downstream merger keeps them as parallel timeline entries rather than collapsing one into the other.

Set "episodic": false (or omit) for stable attributes that describe an ongoing state (preferences, profile traits, opinions, relationships, plans). These can be replaced over time when contradicted.

# SLOT FIELDS — optional, for STABLE attributes that may change over time

When a fact is a stable profile / preference / relationship attribute that may need to be replaced later (e.g. "user lives in Shanghai" should overwrite a prior "user lives in Beijing"), set:

  "subject":   what the fact is about ("user", "user.spouse", "pet:<name>")
  "predicate": snake_case key from the controlled list:
               lives_in, works_at, occupation, birthday, language,
               spouse, child, parent, pet,
               preference.<topic>, status.<topic>
               If none fits, leave both empty.

Episodic facts (episodic=true) MUST leave subject and predicate empty — they are append-only timeline data, not slot replacements.

# EXISTING MEMORIES — dedup only, NEVER silence new facts

If an EXISTING MEMORIES block is provided below, use it ONLY to avoid emitting a fact whose specific content is already captured verbatim or as a near-paraphrase. The block is NOT a topic filter.

CRITICAL: An existing memory that mentions an entity does NOT mean every claim about that entity is already captured. New events, attributes, opinions, or experiences involving a known entity MUST still be emitted as new facts.

GOOD: Existing memory "Alice has a dog named Max." + new turn "Alice took Max to the beach on 7 May 2024." → emit the beach trip as a new fact (the dog ownership is the only thing already captured).

BAD : Skipping the beach trip because Alice + Max already appear in existing memories.

Two facts are duplicates only when the SPECIFIC EVENT / CLAIM is the same, not when they share actors or topics. Different timestamps, different motivations, different outcomes ⇒ different facts.

# FEW-SHOT EXAMPLES

## Example 1 — multi-speaker turn with named speakers + temporal anchor + relationship

Conversation:
user: [2024-03-14 10:30] Alice: Hey Bob, finally went to the climbing gym this morning — my first lead climb in 6 months. Felt amazing.
assistant: [2024-03-14 10:35] Bob: Whoa, proud of you. Did Eve come?
user: [2024-03-14 10:40] Alice: Yeah, my sister Eve belayed me. We hit the coffee shop on the corner right after to celebrate.

Facts:
{
  "facts": [
    {"content": "On 14 March 2024, Alice did her first lead climb in six months at a climbing gym, and described it as feeling amazing.",
     "categories": ["episodic", "events"],
     "entities": ["Alice"], "episodic": true, "source": "user", "confidence": 0.95},
    {"content": "Alice's sister Eve belayed her during her first lead climb at the climbing gym on 14 March 2024.",
     "categories": ["episodic", "relationships"],
     "entities": ["Alice", "Eve"], "episodic": true, "source": "user", "confidence": 0.9},
    {"content": "On 14 March 2024, Alice and her sister Eve went to a coffee shop on the corner right after climbing to celebrate Alice's first lead climb in six months.",
     "categories": ["episodic", "events"],
     "entities": ["Alice", "Eve"], "episodic": true, "source": "user", "confidence": 0.9}
  ]
}

Notice: the speaker prefix names Alice and Bob — facts are attributed to Alice by name, NOT to "the user". The "[2024-03-14 10:30]" prefix grounds "this morning" to 14 March 2024. "My sister" is canonicalised to "Alice's sister Eve". Entities are atomic proper nouns only (no "climbing", no "coffee shop" since it has no proper name).

## Example 2 — facts uttered by the SECOND speaker (real person, not AI)

Conversation:
user: [2024-03-15 19:15] Alice: How did the marathon go yesterday?
assistant: [2024-03-15 19:20] Bob: Finished my first half-marathon — 2h 14m. My knee is killing me but I'm proud.
user: [2024-03-15 19:22] Alice: Amazing!! Did Carol run with you?
assistant: [2024-03-15 19:25] Bob: No, she was at her mom's place visiting. She cheered from the finish line though.

Facts:
{
  "facts": [
    {"content": "On 14 March 2024, Bob ran and finished his first half-marathon with a time of 2h 14m, and was sore in the knee afterwards.",
     "categories": ["episodic", "events"],
     "entities": ["Bob"], "episodic": true, "source": "assistant", "confidence": 0.95},
    {"content": "On 14 March 2024, Bob's partner Carol was at her mother's place during Bob's half-marathon but came to the finish line to cheer him on.",
     "categories": ["episodic", "relationships"],
     "entities": ["Bob", "Carol"], "episodic": true, "source": "assistant", "confidence": 0.85}
  ]
}

Notice how facts uttered by the assistant role about Bob's own life are extracted with the SAME rigour as Alice's facts — Bob is a real person here, not an AI. "Yesterday" in the prefix-anchored turn of 15 March 2024 is grounded to 14 March 2024.

## Example 3 — preference with inference evidence

Conversation:
user: I've been devouring Judith Butler's essays this month. Went to her lecture at Columbia last Thursday too — kept thinking I might shift my major from Psychology to Gender Studies.

Facts:
{
  "facts": [
    {"content": "The user has been reading Judith Butler's essays.",
     "categories": ["preferences"],
     "entities": ["Judith Butler"], "episodic": false, "source": "user", "confidence": 0.85},
    {"content": "The user attended a Judith Butler lecture at Columbia University.",
     "categories": ["episodic", "events"],
     "entities": ["Judith Butler", "Columbia University"], "episodic": true, "source": "user", "confidence": 0.9},
    {"content": "After reading Judith Butler and attending her Columbia lecture, the user considered switching their major from Psychology to Gender Studies — suggesting Gender Studies as a likely academic focus.",
     "categories": ["plans", "opinions"],
     "entities": ["Judith Butler", "Columbia University"], "episodic": false, "source": "user", "confidence": 0.85}
  ]
}

## Example 4 — slot-eligible stable attribute

Conversation:
user: Just moved to Shanghai from Beijing for a new job at ByteDance.

Facts:
{
  "facts": [
    {"content": "The user lives in Shanghai.",
     "categories": ["profile"],
     "entities": ["Shanghai"],
     "subject": "user", "predicate": "lives_in",
     "episodic": false, "source": "user", "confidence": 0.95},
    {"content": "The user works at ByteDance.",
     "categories": ["profile"],
     "entities": ["ByteDance"],
     "subject": "user", "predicate": "works_at",
     "episodic": false, "source": "user", "confidence": 0.95},
    {"content": "The user moved to Shanghai from Beijing for a new job at ByteDance.",
     "categories": ["episodic", "events"],
     "entities": ["Shanghai", "Beijing", "ByteDance"],
     "episodic": true, "source": "user", "confidence": 0.95}
  ]
}

## Example 5 — pure noise, skip

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
		// Back-compat: if the extractor did not emit the explicit
		// episodic flag (older or custom prompts), infer it from
		// the legacy category vocabulary so downstream supersede
		// guards behave the same regardless of prompt vintage.
		if !f.Episodic {
			for _, c := range f.Categories {
				switch strings.ToLower(c) {
				case "episodic", "events":
					f.Episodic = true
				}
				if f.Episodic {
					break
				}
			}
		}
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
					"required": []string{"content", "categories", "entities", "source", "confidence", "subject", "predicate", "episodic"},
					"properties": map[string]any{
						"content":    map[string]any{"type": "string"},
						"categories": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"entities":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"source":     map[string]any{"type": "string"},
						"confidence": map[string]any{"type": "number"},
						"subject":    map[string]any{"type": "string"},
						"predicate":  map[string]any{"type": "string"},
						"episodic":   map[string]any{"type": "boolean"},
					},
				},
			},
		},
	},
}
