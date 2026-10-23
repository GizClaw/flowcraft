# memory eval

Evaluation module for `backends/memory`. It owns the whole eval surface:

- the `eval` package (scenario model, LoCoMo/LongMemEval loaders, `Run` /
  `RunAll`, baseline comparison), and
- the runners under `cmd/`: `memory-eval` (dataset run, optionally against a
  live inference provider through a deploy document), `memory-eval-diag`
  (per-question retrieval diagnostics), `memory-vector-readout` (what one
  retrieval costs inside the vector lane, against a live deployment — see
  "Reading the vector lane"), and the two store readings that never call a
  provider — `memory-fact-density` (how densely a derivation wrote: facts per
  commit, and what a similarity floor or merge would leave of it) and
  `memory-fact-diff` (what a second derivation of one corpus holds
  differently, see "Comparing two derivations").

It is its own Go module so that host wiring (`core/deploy`, provider drivers)
never becomes a dependency of the `backends/memory` library module.

## Setup

```bash
cd backends/memory/eval
curl -sL -o locomo10.json \
  https://raw.githubusercontent.com/snap-research/locomo/main/data/locomo10.json
```

Fill the key the deployment needs in a `.env` file (git-ignored). It can live
next to this module or at the repository root — the live probes look in
`backends/memory/eval/.env` first and the repository root second — and
`-env-file` accepts any path explicitly:

```bash
cp .env.example .env   # or ../../../.env to keep it at the repository root
```

The default deployment uses two providers: DeepSeek for fact extraction
(`DEEPSEEK_API_KEY`, served by the openai driver) and ByteDance Ark
`doubao-embedding-vision` (multimodal: text + image) for the vector lane
(`ARK_API_KEY`). DeepSeek ships no embedding API, so the embedding model is
declared as a separate bytedance provider in `deploy.yaml`; a third provider,
`openai` (`gpt-4o-mini`, also multimodal), is declared but unused unless a run
selects it with `-answer-provider openai -answer-model gpt-4o-mini` or points
`memories.settings.generate` at it. Three consequences worth knowing:

- the multimodal Ark endpoint embeds one item per HTTP request (no batching),
  so a full LoCoMo run issues roughly one call per indexed message, plus one
  per query;
- images are exercised: `-images` defaults to `native`, which downloads each
  turn's image (8-way parallel, 20s timeout, bounded cache), verifies it by magic
  bytes and inlines it as an `ImagePart`. An image over the 4MB inline budget is
  re-encoded to fit rather than dropped (`images_shrunk`), since a caption is not
  a stand-in for the picture the reference harness attaches. The picture reaches the vector lane
  (the embedder takes text *and* image parts); the fact extractor and the answer
  prompt build their requests from `Content.Text()`, so they see the turn's text
  only. A download that fails falls back to the caption annotation. Failures are
  classified and printed (`image_failures{permanent,busy,transient,oversized}`),
  because the classes differ in what they mean for comparability: only a cause
  that describes the url (a 404, a hotlink block, an oversized payload) is
  cached for the next run, while a timeout or a 5xx is retried — caching those
  turned a slow host into a missing image for 24h and made the image set a
  function of the network's luck. `-images annotation` keeps
  the historical text-only shape.
- one image is not cheap on either multimodal endpoint. Measured on Chat
  Completions: a 96×96 PNG billed 8,528 prompt tokens against 28 for the same
  prompt without it, on `gpt-4o-mini` (8,500 tokens for one thumbnail). Ark
  embeds one image per request as well. Ingest images because the vector lane
  needs them, not because the context window is large.

The deployment declares the logical name `doubao-embedding-vision` and the
profile pins the dated snapshot (`doubao-embedding-vision-251215`); Ark also
lists `doubao-embedding-vision-250615`, and an account endpoint ID (`ep-xxx`)
works in the same mapping. Without an Ark key, drop `embed:` and the
`bytedance` provider (or point the provider at a compatible gateway).

## Run

Ad-hoc lexical run (no model, recent + BM25 + entity lanes only):

```bash
go run ./cmd/memory-eval -samples 1
```

Full run through a deploy document with a real provider:

```bash
go run ./cmd/memory-eval -deploy ./deploy.yaml -env-file ../../../.env -samples 1
```

Useful flags: `-samples N` (conversations), `-questions N` (per conversation),
`-max-items` / `-max-tokens` (context budget), `-recent-items` (assembly
`recent.max_items`), `-out report.json`, `-build-only` (verify the deployment
document builds and wires without calling a model).

## Question categories

LoCoMo is scored over the answerable categories only — 1 multi-hop, 2 temporal,
3 open-domain, 4 single-hop. **Category 5 (adversarial) is excluded by design**
and never answered: those questions are unanswerable by construction (the
conversation does not contain the answer and the dataset ships no gold), so
their accuracy is a refusal rate, not the answer accuracy this harness
measures. The loader drops them explicitly and counts them as
`skipped_adversarial` on the `dataset:` line, so the excluded slice is always
visible.

Consequences to keep in mind when reading results: every rate printed here is
conditional on categories 1–4 (for `locomo10.json` that is 1540 of 1986
questions), and any comparison with a published LoCoMo table must say so,
because the official protocol also scores category 5.

## Reference protocol vs this harness

A rate printed here is not a LoCoMo leaderboard number unless the run is
configured to reproduce the reference protocol. The differences, read off
`snap-research/locomo` `task_eval/` (`gpt_utils.py`, `evaluation.py`):

| | reference harness | this harness |
| --- | --- | --- |
| context | the whole conversation, windowed to the model's token budget; every session is prefixed `DATE: <session date_time>` and `CONVERSATION:` | the recalled pack from long-term memory (`-max-items`, `-max-tokens`), each item labelled `[source-class/kind]` |
| prompt | `Based on the above context, write an answer in the form of a short phrase for the following question. Answer with exact words from the context whenever possible.` then `Question: {} Short answer:` (a batched variant when `--batch-size > 1`) | `-answer-style long` (product policy) / `short` (the same short-phrase shape) / `evidence` (quotes first, answer second) |
| category 2 | every temporal question carries the suffix ` Use DATE of CONVERSATION to answer with an approximate date.` | off by default; `-temporal-hint` appends that string verbatim |
| category 5 | asked as a two-way choice between `Not mentioned in the conversation` and the adversarial answer | excluded, counted as `skipped_adversarial` |
| grading | token-F1 with the per-category rules below | `official.Score` re-scores a stored report with those rules (no model calls); live runs add a strict and a lenient judge plus evidence recall |
| scored categories | 1-5 | 1-4 |

Scoring rules, as reproduced by `official.Score`: category 1 splits prediction
and gold on commas and averages the best F1 per gold sub-answer (listing one of
two items scores 0.5); categories 2 and 4 are plain token-F1; category 3 scores
the first `;`-separated segment of the gold only; category 5 is the refusal
check (`no information available` / `not mentioned`). Token-F1 is
`normalize_answer` (drop commas and ASCII punctuation, drop `a/an/the/and`,
lowercase, collapse whitespace) followed by Porter stemming; this module stems
with `reiver/go-porterstemmer` rather than NLTK's, so a last-decimal difference
is a stemmer artefact, not a result.

Two parts of the reference protocol are dataset-label routing rather than
product behaviour, and this harness keeps them out of the default path:

- **the category-2 suffix** sits behind `-temporal-hint`. Measured over the 321
  temporal questions of `locomo10.json` (frozen LoCoMo memory, `-skip-derive
  -answer -answer-style short -judge-style strict`, deepseek-flash answering and
  judging, so the suffix is the only difference between the two runs):

  | | strict judge | official token-F1 | output tokens | mean latency |
  | --- | --- | --- | --- | --- |
  | without the suffix | 234/321 = 72.90% | 62.83% | 191k | 1.33s |
  | with the suffix | 231/321 = 71.96% | 62.03% | 279k | 1.50s |

  The suffix does not buy accuracy on this pipeline. The strict difference runs
  in both directions across the ten conversations (4 scenarios better, 3 worse,
  3 level) and sits inside the ±1 question/scenario noise band; token-F1 slips
  0.8pp even though the visible answers only grow from 3.94 to 4.11 words,
  because most of the extra output is the model deliberating about dates (+46%
  output tokens). Retrieval is untouched, as it must be (evidence recall 88.00%
  vs 88.27%). The flag exists to reproduce the published prompt exactly, not as
  a default to turn on.

- **the fact-extraction prompt** carries no benchmark content: its worked
  examples use invented people and dates. The previous examples were copied
  from LoCoMo - names, dates, and, in the temporal example, one of the
  dataset's own gold answers - which made the extractor partly tuned to the
  benchmark it is scored on. Extraction is unchanged by the rewrite
  (`TestExtractionModelAB`, 30 sampled turns, deepseek-flash: surface coverage
  53.5% → 54.9%, unsupported facts 0% in both, facts per turn 15.10 → 14.37 —
  inside that probe's run-to-run variance). `chat.AlgorithmVersion` moved to
  `1.4.0` with the rewrite, so existing workspaces re-derive rather than mixing
  facts from both prompts.

## Answering policy

The product policy is `-answer-style long` (`answer-policy-v2`). A `v3` of that
policy had it quote its evidence before answering — the step the official
`evidence` shape uses — and was built, measured, and then reverted: the step pays
on the shape it was developed on and not on the product shape. Both experiments
run over the frozen LoCoMo library with everything else held fixed — same
recalled context, same answering model, same two judges, 1540 questions, 30
items / 6144 tokens. The rate columns come from those full runs; the token column
comes from paired 30-question cost probes over the same library (20 items / 4096
tokens, one judge), which are the only runs that price all four prompt shapes
under identical settings:

| | strict | lenient | multi-hop (cat 1) | open-domain (cat 3) | answer out tokens/question |
| --- | --- | --- | --- | --- | --- |
| `answer-short-v1` (official short shape) | 67.79% | 85.91% | 39.7% | 42.7% | 601 |
| `answer-evidence-v1` (short + quoting) | **76.36%** (+8.57pp) | 88.77% (+2.86pp) | **51.4%** (+11.7pp) | **55.2%** (+12.5pp) | 1798 (**3.0×**) |
| `answer-policy-v2` (product shape) | 78.51% | 89.93% | 54.96% | 57.29% | 779 |
| `answer-policy-v3` (product + quoting) | 78.25% (−0.26pp) | 89.68% (−0.25pp) | 53.90% (−1.1pp) | 61.46% (+4.2pp) | 2034 (**2.6×**) |

Read that as two experiments rather than one ladder: the first pair changes the
official short shape, the second changes the product shape, and the two shapes
carry different prompts (the product prompt carries the long-form rules and a
24k-rune context). The 11pp between the short and product rows is not a paired
reading of the flag — the two rows come from different libraries — and the pinned
library below reads those same two shapes at 66.69% and 76.30%.

- **The product shape is worth +9.61pp, measured paired.** Both arms over the
  pinned library (`84154bf9`, `-skip-derive`, 30 items / 6144 tokens, temperature
  0, `-judge-style both`, same answering model): `answer-short-v1` grades 66.69%
  strict / 84.55% lenient, `answer-policy-v2` 76.30% / 87.60%. 181 questions
  improved to 33 regressed, McNemar p<1e-6, and every category moved up —
  multi-hop 36.52 → 50.35 (+13.83, p<1e-6), temporal 75.39 → 79.13 (+3.74,
  p=0.050), open-domain 42.71 → 52.08 (+9.38, p=0.022), single-hop 76.22 →
  86.68 (+10.46, p<1e-6). Evidence recall is unchanged (88.64% → 88.77%, two
  questions), because the read path is not the variable. It costs 2.9× the
  answer side's output tokens (164 → 470 per question on these runs; the
  smaller-context probe above prices the same two shapes at 601 and 779) plus
  ~1.3× the two judges'.
- **The shape is a measurement choice as much as a product choice.** Questions
  whose evidence the recall stage fully delivered convert at 72.4% under the
  short shape and 82.6% under the product shape, so a strict score taken on the
  short shape understates what a deployment running the product prompt answers.
  The remaining 365 strict errors split as: 239 with the evidence in hand (17.4%
  of the questions that had it, of which 127 the lenient judge also rejects) and
  126 where the recall stage was missing evidence. Retrieval would have to become
  perfect to reach 82.55%, so under the product shape the read path is worth
  +6.26pp, and half of it (45 questions) sits in multi-hop alone.
- **Quoting the evidence pays on the short shape, +8.57pp for 3.0×.** Paired
  short against short-plus-quoting, 152 questions improved to 20 regressed,
  McNemar p=2.6e-26; every category moved the same way.
- **It pays nothing on the product shape.** Paired over the 1529 questions the
  two product runs share, −0.26pp, 26 improved to 30 regressed, p=0.69.
  Open-domain is the one category that moved up (+4.2pp on n=96, inside its
  noise band) and multi-hop the one that moved down; the answer side still went
  779 → 2034 output tokens per question, measured by a paired 30-question cost
  probe over the same library (the same probe prices short at 601 and evidence
  at 1798).
- **Retrieval was not the variable.** Per-conversation evidence recall is
  identical in 8 of 10 conversations and differs by a single turn in the other
  two; both arms read the same frozen library (`-skip-derive`, and the report's
  `library` block shows `underived=0 behind=0` for the current policy digest).

So `v3` was reverted: on the product shape the step buys no measurable accuracy
and costs 2.6× the answer side's output tokens. The likeliest reason is that the
long shape already quotes and dates its evidence without being asked — which is
exactly what the step adds to the terse short shape. `-answer-style evidence`
keeps the shape that pays, and a future `v3` has to move a paired product run
before it ships.

The product *prompt template* was reverted with it:
`core/memory/render/default.gotmpl` is back to the `<memory_context>` data block
alone, and the `<memory_instructions>` block that carried the quoting step
(static text beside the data, so `<memory_context>` stayed pure reference data)
is gone. That block was never what the table measures — this harness renders the
recalled pack itself — so a deployment that wants the step back has to add the
template block *and* re-test here first: the short shape's +8.57pp is not
evidence for the product shape.

## The answering model

`-answer-model` was the one knob this harness had never paired, and the product
shape made it worth pairing: 239 questions still failed with the evidence fully
in hand, and "the answerer is too weak" is the obvious reading of that.

The numbers do not support it. Swapping the answerer to `deepseek-v4-pro` over
the same frozen library with everything else held fixed — same prompt, same
recalled context, temperature 0, and the same two `deepseek-flash` judges —
moves the score not at all: 76.30% → 76.10% strict (−0.19pp, 40 questions up to
37 down, McNemar p=0.82) and 87.60% → 87.14% lenient (−0.45pp, 33:26, p=0.44).
Every category stays inside its own noise band, and the report's fingerprint
differs from the flash arm in exactly one field, `answer_model`. The judge is
deliberately not swapped along with the answerer: the arm asks whether a better
answerer clears the residual, and a ruler that moved with the treatment could
not answer that.

Splitting by whether the recall stage delivered the evidence is what makes the
result usable:

| evidence in hand (n=1370) | strict | fixed by the swap | broken by it |
| --- | --- | --- | --- |
| `deepseek-flash` | 82.6% | — | — |
| `deepseek-v4-pro` | 82.0% | 28 | 36 |

211 of flash's 239 in-hand errors stay wrong under `v4-pro` (88%), and 85% of
`v4-pro`'s own in-hand errors were already flash's, so the two arms fail the
same questions rather than trading them. The perfect-recall ceiling — the in-hand
conversion rate, by construction — does not move either (82.6% → 82.0%).

Reading the 211 stored answers says where the loss lives instead:

- 74 (35%) — the answer says the memory does not contain the fact ("the memories
  don't name that book"). The control set, in-hand questions the same arm got
  right, says that 6.5% of the time, so the residual is ~5× enriched for a fact
  that never reached the prompt.
- 42 (20%) — a multi-part gold ("Oliver, Luna, Bailey") answered in part: the
  missing part did not reach the prompt either.
- 6 (3%) — the gold string is in both answers and both judges still said no.
- 89 (42%) — wrong in their own words.

Only the last bucket can be read as answering skill, and it does not shrink when
the answerer gets stronger — including the questions where both arms answer a
coarser neighbour instead of the specific detail the gold wants. So the next
lever is what the prompt is given — which items the pack selects, or how much
detail the derivation keeps — not the model. Every one of the 1540 questions
reads a pack of exactly 30 items, so the budget is a selection and not the whole
memory; the next section doubles it.

What the swap costs: 973 vs 470 answer output tokens per question (2.07×; most of
it is the model's own reasoning, since the visible answers are 29% *shorter*, 208
vs 291 characters), and 59m25s against 1h01m56s for the same 1540 questions —
although this arm ran at `-answer-concurrency 32` and the flash arm at 4, and the
report's per-question latency is queue-inclusive, so call-level latency is not
comparable between the two.

## The recall budget

Arm M closed the answer-model question, so arm N asked the one the residual
raises next: if 239 questions fail with the evidence in hand, is a 30-item pack
showing the answerer too little? The arm doubles the budget — 60 items / 12288
tokens — and raises the cap that renders the pack into the answer prompt with it
(24000 → 60000 runes). The cap has to move with the budget: the 30-item arm's
whole answer prompt averaged 4250 tokens, so 24000 runes never bound, while at 60
items the pack alone averages 7761 (p90 10045) — about 31k runes — and the old
cap would have clipped it, so the arm would have measured the cap. Everything
else is held: the same library, `answer-policy-v2`, the same `deepseek-flash`
answerer and the same two judges, temperature 0. The fingerprint differs in
exactly those three fields.

It is worth +5.32pp, the second-largest effect measured here after the prompt
shape:

| | strict | lenient | evidence hit | multi-hop | temporal | open-domain | single-hop |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 30 items | 76.30% | 87.60% | 88.77% | 50.35% | 79.13% | 52.08% | 86.68% |
| 60 items | **81.62%** | **91.62%** | **95.13%** | **65.60%** | 81.93% | **61.46%** | **89.18%** |
| paired | +5.32 | +4.03 | +6.36 | +15.25 | +2.80 | +9.38 | +2.50 |
| flips | 20:102 | 14:76 | 0:98 | 3:46 | 6:15 | 1:10 | 10:31 |
| McNemar | <1e-6 | <1e-6 | <1e-6 | <1e-6 | 0.078 | 0.012 | 0.001 |

The bigger pack never loses a turn it had — all 98 evidence flips go one way —
and finds 2246 of 2354 evidence turns, up from 2074. Splitting the 1540 questions
by what the pack delivered says where the 82 extra correct answers came from:

| group | n | 30 items | 60 items | flips | McNemar |
| --- | --- | --- | --- | --- | --- |
| evidence in hand in both arms | 1370 | 82.55% | 85.62% | 61:19 | 2.7e-6 |
| evidence the bigger pack gained | 98 | 21.43% | 56.12% | 34:0 | 1.2e-10 |
| evidence still missing | 72 | 31.94% | 40.28% | 7:1 | 0.07 |

Half of the gain (+42 of +82 questions) is not new evidence at all — it is the
same evidence turns with 30 more items of surrounding context, and it is the
significant half. The other half (+34) is questions whose evidence the 30-item
pack had cut. The pack does two jobs at once and only one of them is retrieval.

The ceiling moves the other way. In-hand conversion — the rate a perfect recall
stage would buy — goes 82.55% → 83.65%, so what perfect retrieval is worth falls
from +6.26pp to +2.02pp: 283 errors remain, 240 with the evidence in hand and 43
without. And the 240 still look like the 239 did. Of the 178 that were already
wrong at 30 items and are still wrong at 60, 72 (40%) have the answer say the
memory does not name the fact — the control, in-hand and correct, says that 7.8%
of the time — 23 answer part of a multi-part gold, 4 carry the gold verbatim, 79
are wrong in their own words. Arm M read the same split (35/20/3/42 against
40/13/2/44): a quarter of the in-hand errors are gone and the shape of the rest
has not changed.

That shape has a mechanism, and `memory-eval-diag` measures it: of the 2238
packed evidence turns, 1910 (85%) arrive as a derived item whose sources cover
the evidence turn's whole commit — the prompt carries a paraphrase, not the
wording the gold is scored against. A fact can therefore be "in hand" by turn id
and still not be legible in the item that stands for it, which is what the 40%
absent bucket looks like.

Cost: the answer prompt goes from 4250 to 8913 input tokens per question (2.10×)
and the answer side from 470 to 534 output tokens (1.14×); the two judges barely
move, since they read the answer and not the pack. Wall is 1h09m47s against
1h01m56s, but this arm ran at `-answer-concurrency 32` and the 30-item arm at 4,
so per-question latency rose by more than the wall clock shows. `-max-tokens
12288` never binds at 60 items (max 11785): the item cap is the knob, and every
question chose from a pool of 188, with the evidence a 60-item pack misses
sitting at pool rank p50 82 — 90 items would reach the median missed turn.

## What a report stores

The fingerprint names the configuration; the report also carries the material and
the price, because a rate without them cannot be compared with the next run:

- `loader` — the dataset conversion, including `images_attached` /
  `images_failed` / `images_shrunk` and the `image_failures` breakdown by cause;
- `usage` — per-role (`answer`, `judge`, `lenient_judge`) calls and input/output
  tokens, so a protocol change can be priced from the stored result;
- `library` — the derivation state the answers were read from: the watermark
  digest and, per scope, how many conversations carry a watermark under *this*
  policy digest and how many are `underived` (their facts, if any, were written
  by another generation of the derivation policy).

`library` exists because `derive=reused` answers from whatever the workspace
holds while the fingerprint's `policy_digest` describes the code: a workspace
derived under an older extraction policy reports every conversation as
underived, and the run warns on stderr when `-skip-derive` is used that way.
Without it, two runs over one workspace and two derivation policies carried the
same fingerprint.

The fingerprint's `values` map also gained `rerank`: the *effective*
`retrieval.rerank` setting, read off the deploy document. `rerank_policy` names
the linked-in policy version whether or not retrieval calls it, so two runs that
differed in that switch used to share a fingerprint.

`retrieval` closes the same gap for the rest of the retrieval side: lane weights
and fusion, the bm25 parameters, `source_quotes` and `rerank`, read back off the
built assembly (`Assembly.RetrievalSummary`, defaults applied) as one JSON
object. The derivation policy digest covers what memorization *writes*; this
covers what retrieval *reads it with*, as a value the report itself names
instead of one hidden inside the deploy document's byte digest. A knob added to
the retrieval settings types appears here without a second edit.

The profile block joins that object only while it is served: a deployment that
leaves `profile.enabled` off serves exactly what it served before the block
existed, so its footprint is the one it had. `maintain` names whether a pass
ran; the block's settings alone do not say whether anything filled it.

`recent_items` and `recent_tokens` are the *effective* recent window for the same
reason: they are read back off the built assembly (`Assembly.RecentSettings`,
defaults applied) instead of the `-recent-items` flag. A deploy document sets
`recent.max_items` itself, so the flag named a value no run used — every run
through `deploy.yaml` stamped 20 while serving 8, and a comparison of two runs
that differed only in that setting would have agreed. Both values change with
this fix, so a report stored before it disagrees with a rerun until it is
regenerated.

`evidence_matching` records the rule evidence recall was graded by, which the
workspace decides: `turn-id` when ingest tagged the messages with their dataset
turns, `committed-text` when the workspace predates that tagging (the run warns
on stderr). The two rules do not score the same (see "Two grading modes"), so a
report stored under one disagrees with a run under the other.

## Two grading modes

The default run grades **evidence recall**, the metric the LoCoMo protocol
reports: the loader keeps each question's `evidence` turn ids, and a question
counts as a hit when every turn that carries the answer was surfaced. A turn is
surfaced either when a recalled item contains its committed text (the turn's own
message, or a derived item a `source_quotes` fold put that text into) or when
the item's provenance resolves to it (a fact points at the messages it was
derived from — the runner wires that through the message store). Results
therefore carry both numbers: turn-level `evidence recall` and
`questions_with_full_evidence`.

Ingest also tags every committed message with the dataset turn it was loaded
from (`DatasetTurnMetadataKey`), and that tag decides the match first: an item
whose sources name the turn counts, whatever the text says. Text alone is not
enough for a turn that carries an image, because such a turn has two renderings
and the store holds one of them: `-images native` keeps the caption out of the
stored text (the picture rides along as a part), while `-images both` and
`-images annotation` fold `[shared image: caption]` into it — and `annotation`
is the default a loader that names no mode gets. Matching by text is therefore
only as good as the agreement between the loader's flag and the mode the store
was written with, and the store does not record that mode.

The diagnostic prints what that costs, as `by-text` beside `packed`. Run against
the store `deploy.yaml` names, and again with `-images annotation`:

```bash
go run ./cmd/memory-eval-diag -deploy ./deploy.yaml -env-file .env -skip-derive \
  -images native -samples 1 -max-items 30 -max-tokens 6144 -top 0
```

Measured over the 152 questions of conv-26 at 30 items (4560 packed items, 201
evidence turns), against the two stores this was developed on — the one
`deploy.yaml` names, written before ingest tagged turns, and one written with
`-images native` after:

| the store                              | loader       | packed turns | found by text alone |
| -------------------------------------- | ------------ | ------------ | ------------------- |
| `deploy.yaml`'s, untagged              | `native`     | 185          | 185                 |
| the same store                         | `annotation` | 128          | 128                 |
| ingested native, tagged                | `native`     | 181          | 181                 |
| the same store                         | `annotation` | 181          | 121                 |

The first two rows are what the flag costs when nothing but text can name a
turn: 57 of the 185 packed turns vanish, and the run reports evidence missing
that its prompt held — that is the measurement the earlier notes recorded as
items matched only after the caption was stripped. The last two rows are what
the tag buys: the pack is the same 181 turns under either flag, and `by-text`
says how much of it the tag alone found (60 turns, the ones whose caption the
annotation-mode loader renders and a native store does not hold). Saying it the
other way: with ids in the store, `-images` no longer decides coverage.

The tag only reaches a workspace ingested after it existed. The sessions commit
under fixed idempotency keys, so re-ingesting into an old workspace replays the
original commits and writes nothing: comparable numbers need a fresh workspace
(the runners and the live probes warn or fail when the store they read carries
no ids).

Read the second kind for what it is: a fact's sources are the whole commit it was
extracted from (`chatSource` in `worker/processor.go` records every message of
one), not the individual turn it paraphrases. A provenance hit therefore says an
item *from the evidence turn's commit* was packed, which is why questions whose
evidence sits in one commit recall so well — any fact out of that session
counts — while questions that need turns from several commits do not.

It also says the pack carried no item with the turn's own wording, wherever the
pack put one: the classification is about the pack, not about pack order, so a
fact derived from the session cannot label a turn a paraphrase while the turn's
own message sits further down the same prompt. Measured on a 60-question run:
86 of 96 evidence turns were packed, and scoring them per item reported 26 as
raw and 60 as provenance — 23 of those 60 with the turn's own message packed
too. Ranking the wording first reports 49 raw and 37 provenance over the same
packs, with `evidence recall` unchanged. Reports stored before this differ in
exactly these two columns, and nowhere else.

Questions without evidence ids fall back to expectation containment (the gold
answer appearing verbatim in the recalled context), which is what LongMemEval
rows use.

`-answer` adds the generative stage: the answering model reads the recalled
context and answers, and a judge model accepts paraphrase, synonyms, and
equivalent dates. Flags:

```bash
go run ./cmd/memory-eval -deploy ./deploy.yaml -env-file ../../../.env \
  -max-items 40 -max-tokens 8192 -answer
```

- `-answer-provider` / `-answer-model` (default `deepseek`/`deepseek-flash`:
  this endpoint also serves `deepseek-v4-pro`, and any other name is silently
  substituted, which `TestConfiguredModelsExistInCatalog` guards against)
- `-judge-provider` / `-judge-model` (default: the answering model)
- `-judge-style strict|locomo|both` — `strict` asks for a bare `CORRECT` /
  `INCORRECT` token, `locomo` mirrors the leaderboard prompt (`{"correct":…}`).
  Only those two shapes are accepted; hedged prose is **ungraded** rather than
  guessed at, and ungraded answers leave the denominator (reported as
  `ungraded_answers`).
- `-answer-style long|short|evidence` (default `long`) — `long` is the product
  policy, `short` reproduces the official LoCoMo prompt shape that token-F1 is
  defined over, and `evidence` is `short` with the quoting step added. All three
  carry their own prompt version in the fingerprint. See "Answering policy"
  below for what the quoting step measured.
- `-temporal-hint`: append the reference harness's category-2 suffix verbatim
  (` Use DATE of CONVERSATION to answer with an approximate date.`) to temporal
  questions. Off by default and recorded in the fingerprint; see
  "Reference protocol vs this harness" for what it measured.
- `-temperature` (default `0`): the sampling temperature of **both** the answer
  and the judge call, recorded in the fingerprint as `temperature`. A negative
  value passes the provider's default through and prints a warning, because
  that is how every report before 2026-10-20 was measured: at the provider
  default one configuration re-run over one library flips 93 of 1540 graded
  questions (see "Measurement noise"). Pinning it makes a re-run agree with
  itself; it does not make a sampled *derivation* deterministic -- that side is
  `generate.temperature` in the deployment (see `../README.md`).
- `-no-date-prefix`: convert turns without the session date prefix the loader
  otherwise writes into each turn's text (`[1:00 pm on 1 May, 2023] Alice: ...`
  → `Alice: ...`); the two conversions differ in exactly this step, so running
  with and without it measures what the prefix contributes to a temporal
  score. Recorded in the fingerprint as `date_prefix`.
- `-observed-at=off|metadata`: `metadata` writes each session date time
  into the messages as `observed_at` metadata (RFC3339 UTC) instead of
  text -- the anchor shape a prefix-less production path receives, and the
  shape the assembly reads by default: `observed_at_anchor` is on, so the
  anchor reaches the extraction source and the answer-side evidence. It
  requires `-no-date-prefix` (the two are renderings of one fact, never
  both). Recorded in the fingerprint as `observed_at`; the loader stats
  report how many messages carried an anchor.
- `-resume`: skip scenarios already recorded in `-out`, so an interrupted
  long run continues where it stopped. Each scenario is retried up to three
  times before the run fails. Results are stamped with a fingerprint (code
  revision, dataset digest, derivation policy digest, judged flags); resuming
  into a report with a different fingerprint is refused unless
  `-resume-anyway` is passed.
- `-answer-concurrency N` (default 4): questions are processed in parallel.
  This is a schedule knob only — each question keeps its own request and
  prompts, retrieval is read-only once derivation is done, and results are
  merged in question order, so a report matches a sequential run for a
  deterministic model. It is deliberately absent from the fingerprint, so
  raising it never invalidates a resume. Runner/answerer/judge implementations
  must be safe for concurrent use.
- `-prepare-all`: ingest every selected scenario, run **one** derivation pass,
  then answer each scenario. `derive.concurrency` fans out over the
  conversations that have pending commits, and a per-scenario run only ever has
  one — so this is the mode that actually parallelises derivation during an
  eval. A derive pass is resumable (a failing conversation keeps its watermark),
  so the pass retries up to three times. The schedule is recorded in the
  fingerprint, and both schedules have been checked to retrieve identical items
  for a conversation with `MEMORY_EVAL_LIVE=1 go test ./cmd/memory-eval/`.
- `-skip-derive`: answer from the derivation the workspace already holds —
  no ingest, no derive pass, retrieval and answering only. This is the cheap
  way to compare two **answering** models against one frozen memory: both runs
  read the same recalled context, so a difference is the model's, not the
  derivation's. It is not a way to change what is remembered: facts, summaries
  and projections are whatever the last derivation left behind, and the
  fingerprint records `derive=reused` so a report from this mode cannot be
  mistaken for a full run (or resumed into one).
- `-maintain`: run one maintenance pass per scope between deriving and
  answering. It is what a run that measures a pass owes the store: the pass
  saves the read-path overlay (superseded and decayed facts) and fills the
  resident profile block, and without it an enabled profile block is served
  empty -- the run says so on stderr rather than reporting a blockless read path
  as the block. The pass runs after the derivation, so it needs `-prepare-all`
  (or `-skip-derive`, to pass over a stored derivation); the fingerprint records
  `maintain=on|off`, and the block's own settings are in the `retrieval` value
  when the deploy document enables them.

### Reproducibility

Every run prints and stores a fingerprint, and `-baseline` warns when the
baseline was measured under a different configuration — comparing across
fingerprints compares two experiments, not a regression. Build with
`-buildvcs=true` (`go build -buildvcs=true -o memory-eval ./cmd/memory-eval`)
so the revision is stamped into the binary; without it the runner asks `git`
directly.

`deploy.yaml` is a template: DeepSeek (responses surface) for fact extraction
and ByteDance Ark `doubao-embedding-vision` for the vector lane. See
"Diagnosing one question" for the retrieval-side diagnostic.

## Diagnosing one question

A report says that a question's evidence did not reach the prompt. It does not
say whether retrieval ever had it, and that is the difference between a ranking
problem and a coverage problem. `memory-eval-diag` prints which one it is, one
question at a time: the pack's source classes and item kinds, where every
evidence turn the dataset records ended up, and the top long-term items with the
dataset turns their sources stand for.

```bash
go run ./cmd/memory-eval-diag -deploy ./deploy.yaml -env-file .env \
  -skip-derive -categories 1 -probe-items 300
```

- `-probe-items N` is what separates the two failure kinds. The pack budget and
  the candidate depth retrieval reads are coupled — the provider reads three
  candidates per packed item — so "did retrieval ever have this turn" can only be
  asked by asking for a much larger pack. The probe re-reads each question at `N`
  items and reports where the turn ranked there: `pool-only` means retrieval had
  it and the budget cut it (the rank says how far the pack would have to reach),
  `absent` means it is outside the depth probed. Without the flag a turn that
  missed the pack is reported as `missing` and says nothing about retrieval.
  The rank is a bound rather than a measurement: the top of the ranking is
  stable, near-ties below it can still move (see "Measurement noise").
- `-link-reach` walks the fact links the write side stores (`linked_memory_ids`)
  one hop out of the pack, and a missed evidence turn that lands inside the walk
  is marked `+1hop`. The summary's `one hop` count is the ceiling a read-side
  link expansion could reach on this sample — the walk reads what the store
  already holds and changes nothing about retrieval. It starts from the pack,
  not from the probe: a probe wide enough to call a turn absent has it by
  construction, so its pool cannot answer what one hop out of the real retrieval
  would add. Run it with or without `-probe-items`; the probe only splits the
  misses further, it does not change the walk.
- `-skip-derive` diagnoses the derivation the workspace already holds — the one
  the run under investigation read — instead of ingesting and deriving first.
  The header prints the library state, and warns when the store's derivation
  belongs to another policy generation.
- `-images` decides the text an evidence turn is *displayed* by, which is what
  an untagged workspace is matched by: there the flag has to match the mode the
  library was ingested with, and a caption the stored turns do not carry hides
  every image turn. A store written after ingest tagged messages with their
  dataset turns is matched by the tag first, so the flag no longer decides
  coverage — `packed` stays put while `by-text` moves, which is the column that
  answers "how much of this did the tag carry?". The header says which of the
  two kinds of workspace the run is in. Under `-skip-derive` the mode shapes the
  text alone: the loader attaches each image as a reference to its url instead
  of downloading it, because a diagnosis never reads a picture, and a fetch that
  fails today would otherwise change the index one run at a time.
- `-questions` / `-categories` select the sample, `-top` and `-snippet` the print
  shape. The summary at the end aggregates the sample and is the part worth
  reading first: it splits the evidence turns into packed / pool-only / absent,
  reports how much of the packed evidence arrived as raw text rather than as a
  derived item's provenance, and reports how much of the packed set committed
  text alone would have found (`by-text`, see "Two grading modes").

## Comparing two derivations

A re-derivation writes one corpus a second time: a different extractor prompt, a
batch budget, a repair after an `AlgorithmVersion` bump. The question it raises
is which claims the new write actually changed, and `memory-fact-diff` answers it
by reading both stores and comparing them batch by batch. The facts of one
source commit are one extractor call, so the two derivations' batches join
through the provenance the facts carry; inside a batch the facts are paired by
the shared token measure (`internal/textsim`: Jaccard, or containment when one
text already says the other — an equal `CanonicalHash` is the same claim
outright).

```bash
go run ./cmd/memory-fact-diff -baseline ./workspace-a -candidate ./workspace-b -json
```

Two derivations never agree word for word — the extractor samples — so the
reading is shaped to tell a claim that went away from one that only changed
wording:

- pairing is one-to-one and greedy over the most similar pairs first: a batch's
  facts are distinct claims, and one candidate fact must not stand in for two.
- `-match` (default `0.7`) is the similarity at or above which two facts count
  as the same claim. Every difference prints beside the closest fact the other
  side wrote and that similarity, and the summary's bucket line splits the
  removed facts by how near their closest counterpart was: past the threshold is
  a claim the other side already carries beside a better-matching fact, the
  bucket just below is wording, and the far buckets are claims that went away.
  Read the buckets before the raw removed count — two honest derivations of one
  batch already differ.
- `-cap` reports what a batch-size cap would have cut on the baseline's own
  batches: how many of them are over it and how many facts that leaves out.
  That is the size of the lever, not a prediction — the extractor writes a
  different batch once the prompt names the cap.
- both sides' headers print facts, batches and the batch sizes' percentiles; the
  batches that differ are listed largest first, and `-limit` / `-snippet` shape
  the print. `-json` keeps the whole reading, including
  `batches_only_in_baseline` / `batches_only_in_candidate` — the source commits
  one side never derived, which is how a derivation still behind is located.
- `-runtime` / `-user` / `-agent` select the scope.

Reads only: no model call, no derivation, no write.

## Reading the vector lane

The vector lane serves by an exact scan: it materializes the scope's table,
embeds the query, walks every entry a filter admits, and sorts what it scored.
Which of those terms dominates decides whether an approximate index could be
worth its loss of determinism, and `memory-vector-readout` answers it by running
the retrieval arm against a live deployment and reading the lane's own counters
(`Assembly.VectorLaneStats` — atomics on the serving path, so the reading
changes nothing the lane serves).

```bash
go build -o zz-memory-vector-readout ./cmd/memory-vector-readout
./zz-memory-vector-readout -deploy ./deploy.yaml -env-file .env \
  -workers 4 -max-items 30 -max-tokens 6144 -out reading.json
```

It never ingests, never derives, and never answers: it reads the workspace the
deploy document names, so the reading describes that derivation. What it
reports:

- a **cold pass**: the first question of each scope, sequentially — the question
  itself (`latency`, and the table it had to materialize: `load`, `misses`,
  `entries`) beside the warm the deployment's own writes paid (`warm`,
  `warms`);
- a **hot pass**: every question the library holds, with `-workers`, attributed
  by the delta of two lane readings taken around it;
- the **phases** inside the lane — table load, query embed, scan (the walk over
  the entries the filter checks plus the cosine over the entries it admits),
  sort — as time and as shares of the lane, of the provider's search stage and
  of the summed request time;
- a **projection**: the measured per-entry scan cost at table sizes the run did
  not have (the scan is linear in entries, the sort n log n), read against this
  run's mean query embed — the term an approximate index would remove against
  the one it would not;
- the **footprint**: the resident materialized tables (scopes, entries,
  dimensions, vector payload) with the process's heap and RSS before and after.

The control store's reading (9,324 entries per scope, 2,048 dimensions, the
documented budget, 1,540 questions, 4 workers on a 4-vCPU host) says the
**embed is the lane**: 159.5 ms/query, 92.3% of the lane's time. The **scan** is
9.7 ms/query — 5.6% of the lane, 0.58% of the search stage, 0.51% of the summed
request time — at 1,035 ns/entry, and the **sort** 3.7 ms/query. Projected at
this run's per-entry cost the scan reaches a quarter of the embed at 40k entries
per scope and two thirds at 100k, so at this corpus's scale the exact scan is
not the term worth trading determinism for. The table costs 76,382,208 bytes of
vectors resident (72.8MB) and ~352MB of heap around its first read, which a cold
pass pays once per process: 14.0s on that host, 9.6s on a laptop reading the
same store.

What makes the numbers worth trusting is that the reading carries its own
serving proof, and states it: the hot pass prints `self-certified` when
`queries = scans = questions`, `embeds = queries` and no lookup failed, and the
JSON keeps the verdict under `certification`, with the disagreements named when
it does not certify (`entries_scanned` against the table size times the
questions asked remains the manual form of the same check). A lane whose stored
state exists but cannot be read is not a lane that found nothing: the request
fails instead of being answered from the remaining lanes (issue #651), so a
pass that lost a lane stops the run, naming the lane and the read that failed:
the error is rendered down its chain, because a memory error states only its
kind and capability. `snapshot_errors` and `warm_errors` count the lookups that
returned an error, apart from the hits and the loads.

## Module dependency

`backends/memory` is not tagged yet: it is workspace-only for now, so `go.mod`
pins it with a local `replace ..`. When the backend is published, bump the
require to the released version and drop the replace (the `examples/forge`
shape). Tracked by #638.

## Measurement noise

Rerunning the same configuration against the same workspace does not return
byte-identical retrievals. Measured with `MEMORY_EVAL_REPEAT=2`
(`TestAnswerContextSize`): 25/34 questions returned a different item list
between two passes, but the **first difference always appeared at index 10–27
of the 30 packed items** — the top of the list is stable, the middle and tail
swap items whose scores are near-ties.

Everything inside the process is deterministic (fusion breaks ties on a full
identity key, every lane sorts by item id before scoring, the packer ends on
`left.ID < right.ID`), so the remaining variance is the **query embedding
call**: a last-bit difference reorders near-ties around the pack boundary. Its
effect is ±1 evidence turn, which showed up as 4/10 scenarios differing by
0.5–0.9pp in recall between two full runs.

Consequences for reading results:

- noise band: **±1 question per scenario**, **<1pp** on answer/F1 rates
- treat any delta inside that band as noise; only larger differences are signal
  (the conclusions this harness produced — the answer-policy change at +9.5pp,
  native images at +4.9pp, the latency work at 19.5× — are all far outside it)
- if a future experiment needs sub-pp resolution, widen the candidate pool
  (3× → 5×) so the packed 30 stop sitting exactly on the ranking boundary

That band describes a run that **reads an existing derivation**. Two further
layers sat outside it before the sampling was pinned, measured over the full
1540-question corpus by pairing every question across runs (McNemar exact):

| layer | retrieval (`hit`) | strict answer | reading |
| --- | --- | --- | --- |
| same library, re-run | 0:1 flips | 49:44 flips, net +5 | ±0.3pp net, but 6% of questions flip ⇒ ±1.2pp on a single draw |
| fresh derivation, same revision | 60:38 flips, net +22 = **+1.43pp** | 88:111, net −23 = −1.49pp | the fact set is a draw: 3424 vs 3090 facts ⇒ **±1.5pp floor** |

`library.digest` does **not** separate the two layers: it hashes the derivation
cursors (scope, conversation, watermark), not the facts, so four runs over three
different fact counts can carry one digest. Sharing a library has to be proved
by `-skip-derive`, which is how an A/B arm avoids the second row entirely.

## Live probes

Every probe below is an opt-in test; `make memory-eval-probes` runs them all
with `MEMORY_EVAL_LIVE=1` (they self-skip without credentials, so `make ci`
stays offline and free).

| probe | answers |
| --- | --- |
| `TestConfiguredModelsExistInCatalog` | is every model named in `deploy.yaml` actually served? |
| `TestExtractionModelAB` | is a candidate generate model good enough to extract? (failures, facts/turn, surface coverage, unsupported facts) |
| `TestAnswerContextSize` | how much context reaches the answer prompt |
| `TestEvidenceFidelity` | how much of the recalled evidence is raw text vs a fact paraphrase |
| `TestRetrievalBudgetSweep` | what evidence coverage each item budget buys |
| `TestPackingRedundancy` | what the packed items actually are: how many distinct commits the slots cover and how many carry the question's evidence |
| `TestDeriveConcurrencyLiveMatchesSequential` | does `derive.concurrency` change what is derived |
| `TestCoIngestedConversationsDoNotChangeRetrieval` | does the two-phase schedule change recall |
| `TestRejudgeStoredAnswersWithLenientJudge` | re-grade a stored report with the other judge style |
| `TestIngestTagsDatasetTurns` | does ingest tag every committed message with its dataset turn, and does a re-ingest repair a store that predates that tagging |

The three retrieval probes read the same workspace the runner does and match
evidence the way the report does, so they need one that carries dataset turn ids
and they fail with the reason rather than report an untagged workspace as missed
recall (`requireTurnIDs`). `TestIngestTagsDatasetTurns` is the exception: it
ingests a conversation into a workspace of its own, because that is the only way
to get a store carrying ids out of a suite that must not derive a second
conversation.

Credentials resolve from `eval/.env` or the repository root; `MEMORY_EVAL_REPORT`
selects the report the re-judge probe reads.
