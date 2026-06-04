// Package words contains narrow lexical helpers for recall.
//
// The package is deliberately not a semantic parser. Callers use these
// functions for structural query features, normalization, and noise filters
// around LLM-produced or structured data: for example, detecting explicit
// temporal/numeric questions, canonicalizing token surfaces, or rejecting weak
// entity anchors.
//
// Keep additions narrow and high precision. Complex meaning and ranking
// decisions should live in assessment, evidence grounding, or dedicated parsers
// such as memory/text/timex.
package words
