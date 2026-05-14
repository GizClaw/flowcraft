package locomo

// LocoMoAnswerPrompt is the QA prompt fed to AnswerLLM. Intentionally
// neutral — earlier versions had three "EM-friendly" rules (force
// minimal answers, force date-format mirroring, suppress IDK) that
// shifted bench numbers without reflecting real memory quality.
// Current version keeps only the two rules actually required for
// grounded QA: answer from MEMORIES (not prior knowledge) and
// paraphrase rather than verbatim-copy.
//
// The extractor side intentionally has no LoCoMo-specific overlay:
// every architectural rule a long-term memory extractor needs lives
// in [sdk/recall.DefaultExtractPrompt] so it ships to every product
// built on FlowCraft, not just to this bench. Re-introducing a
// LoCoMo-only extractor prompt would risk silent drift between eval
// scores and production behaviour.
const LocoMoAnswerPrompt = `Answer the QUESTION using only the MEMORIES below.

Guidelines:
- Ground the answer in the memories; do not invent facts that are not supported.
- Paraphrase in your own words rather than quoting verbatim.
- If the memories don't contain enough information, say so.

%s

Answer:`
