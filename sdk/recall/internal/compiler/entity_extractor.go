package compiler

// EntityExtractor mines entity mentions from a fact's natural-language
// content during the Structurizer stage. It is intentionally a
// separate interface from the late-pipeline EntityResolver: the
// resolver canonicalises mentions against the alias store; the
// extractor surfaces the mentions in the first place.
//
// Per-run diagnostics show this stage fires on ~100% of LLM-extracted
// facts, so it is THE load-bearing Structurizer sub-task. Any
// improvement in entity precision / recall propagates directly to
// the entity, graph, and profile retrieval lanes — which is why the
// implementation is pluggable in the first place.
//
// Contract:
//   - content is the natural-language sentence the LLM emitted.
//   - known is the entity-projection snapshot of canonical mentions
//     the SDK has already seen in this scope; implementations
//     should fold matching surface forms into the output to avoid
//     fragmenting the entity graph.
//   - Returned values must be lower-cased and deduped; ordering is
//     up to the implementation but stable orderings are nicer for
//     diagnostics / golden tests.
type EntityExtractor interface {
	ExtractEntities(content string, known []EntitySnapshot) []string
}

// RuleBasedEntityExtractor is the default deterministic extractor.
// It combines two rule families:
//
//  1. Title-Cased token NER over the content (heuristic English
//     proper-noun detection — see extractEntities for the exact
//     stopword + tokenisation rules).
//  2. Substring matching against KnownEntities' canonical and alias
//     surface forms so previously-seen entities get folded back into
//     the fact even when the LLM did not Title-Case them.
//
// Known limitations (the reason this is a swappable interface):
//   - English-centric: capitalisation is the primary NER signal,
//     so non-Latin scripts and lower-case proper nouns are missed.
//   - No disambiguation: two different "Bob"s collapse to one
//     canonical entity.
//   - No multi-word phrase detection beyond alias matching.
//
// Production deployments needing non-English content or entity
// disambiguation should plug in a custom EntityExtractor that calls
// an external NER service (spaCy, an LLM, a cross-encoder).
type RuleBasedEntityExtractor struct{}

// ExtractEntities implements EntityExtractor.
func (RuleBasedEntityExtractor) ExtractEntities(content string, known []EntitySnapshot) []string {
	return extractEntities(content, known)
}
