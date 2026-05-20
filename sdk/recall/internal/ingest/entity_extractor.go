package ingest

import "github.com/GizClaw/flowcraft/sdk/recall/internal/port"

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

var _ port.EntityExtractor = RuleBasedEntityExtractor{}

// ExtractEntities implements port.EntityExtractor.
func (RuleBasedEntityExtractor) ExtractEntities(content string, known []port.EntitySnapshot) []string {
	return extractEntities(content, known)
}
