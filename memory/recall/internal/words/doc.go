// Package words contains lexical surface helpers for recall.
//
// The package is deliberately not a semantic parser. Callers use these
// functions for conservative guards, query features, and noise filters around
// LLM-produced or structured data: for example, checking whether direct
// evidence has an obvious negation cue before accepting a negated assertion,
// detecting bridge/collection-style query surfaces, or rejecting weak entity
// anchors.
//
// Keep additions narrow and high precision. Complex meaning, temporal
// reasoning, and modality decisions should live in the extractor contract,
// evidence grounding, or dedicated parsers such as memory/text/timex.
package words
