// Package fact stores immutable derived facts per derivation generation.
//
// A fact is derived, not canonical: it is a function of the canonical messages
// and of the derivation policy -- prompt, model, and algorithm versions -- that
// read them. The generation is that policy's identity, and it is part of every
// fact key, so re-deriving under a new policy writes beside the generation it
// replaces instead of merging into it. The same content identity may therefore
// exist once per generation without conflicting, which is what keeps the keys
// stable across a policy change.
//
// The package separates the two planes that read facts:
//
//   - Derivation reads the generation it is building, through an explicit
//     ListOptions.Generation, and writes into that same generation. Its reads
//     are scoped by the caller, so no deriver can be affected by whichever
//     generation happens to be published.
//   - Readers resolve the conversation's published generation, through
//     ActiveGeneration, and never name one. Facts of a generation no completed
//     pass published stay stored but invisible.
//
// Add merges a re-derived fact into the fact it duplicates by canonical
// content. That identity is contents-based, so the merge state of a fact depends
// on the order in which its generation was built -- and that is deterministic,
// because one generation walks its commits in order. Nothing here reads across
// generations: a generation is a snapshot of one policy's derivation, and the
// merge state of one generation can never depend on another's.
package fact
