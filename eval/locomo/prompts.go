package locomo

// LocoMoExtractorPrompt is tuned for LoCoMo-style multi-session
// conversations:
//   - turns are pre-formatted as "[<datetime>] <speaker>: <text>" by
//     the converter, so the model can attribute each fact to a date
//     and a person.
//   - we explicitly ask for many small facts (LoCoMo conversations
//     span 35+ sessions; capping at ~20 facts/conv silently drops
//     90% of the corpus).
//   - timestamps must be embedded inline so retrieval-time text
//     matching can surface them for cat2 (temporal) questions.
//
// Exposed at package level so the cobra `eval locomo run` wiring can
// reference it; older binaries embedded the same text directly in
// cmd/eval/main.go.
const LocoMoExtractorPrompt = `You are a long-term memory extractor for multi-session dialogues. Read the conversation below and extract every distinct, self-contained fact.

GUIDELINES
1. Emit ONE fact per atomic claim — preferences, profile attributes, events, plans, decisions, opinions, relationships. Do not bundle multiple claims.
2. ALWAYS embed the date the fact was said in the content itself (e.g. "On 7 May 2023, Caroline went to an LGBTQ support group"). Use the timestamp prefix [<datetime>] that appears at the start of each turn.
3. Attribute facts to the SPEAKER by name, not "the user" or "the assistant" — both speakers contribute facts in this dialog format.
4. Do NOT deduplicate. Different time points of the same kind of fact are separate facts.
5. Skip pure greetings / acknowledgements / single-emoji turns.
6. Be exhaustive: a 30-session conversation should produce 100+ facts.

OUTPUT FORMAT — strict JSON object with a single "facts" array, no prose, no fences:
{
  "facts": [
    {
      "content": "On 8 May 2023, Caroline mentioned she joined an LGBTQ support group.",
      "categories": ["episodic", "events"],
      "entities": ["Caroline", "LGBTQ support group", "8 May 2023"],
      "source": "user",
      "confidence": 0.95
    }
  ]
}

If no facts: return {"facts": []}.

%sCONVERSATION:
%s
`

// LocoMoAnswerPrompt is the QA prompt fed to AnswerLLM. Intentionally
// neutral — earlier versions had three "EM-friendly" rules (force
// minimal answers, force date-format mirroring, suppress IDK) that
// shifted bench numbers without reflecting real memory quality.
// Current version keeps only the two rules actually required for
// grounded QA: answer from MEMORIES (not prior knowledge) and
// paraphrase rather than verbatim-copy.
const LocoMoAnswerPrompt = `Answer the QUESTION using only the MEMORIES below.

Guidelines:
- Ground the answer in the memories; do not invent facts that are not supported.
- Paraphrase in your own words rather than quoting verbatim.
- If the memories don't contain enough information, say so.

%s

Answer:`
