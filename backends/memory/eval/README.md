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

The default deployment reaches every model it declares through one gateway
(`https://ai.haivivi.cn/v1`, `GATEWAY_API_KEY`) and declares two providers
because it speaks two wires there, not because it reaches two vendors: fact
extraction and answers run `deepseek-flash` over the openai driver, the vector
lane runs `doubao-embedding-vision` (multimodal: text + image) over the bytedance
driver, whose base_url is what points it at the gateway. The same credential
also serves a second generate model (`deepseek-v4-pro`, text-only, selected with
`-answer-provider gateway -answer-model deepseek-v4-pro`) and the decision
surface (`azure-decision`) the maintenance arbiter asks; `deploy.yaml` declares
all four names so `TestConfiguredModelsExistInCatalog` holds the document to the
endpoint's catalog, and a name the gateway does not authorize answers 403
`service_model_unavailable` rather than being substituted.

There is no second vendor and no second key. The document used to declare an
`openai` provider (`gpt-4o-mini` at `api.openai.com`) as the alternative answer
backend, which made `OPENAI_API_KEY` a build-time requirement for a model no
harness run selects; the gateway now serves both a second generate model and the
decision surface, and `gpt-4o-mini` is scheduled to retire on 2027-01-14, so
the provider is gone and the alternative runs on the same key. Three
consequences worth knowing:

- the multimodal embedding route embeds one item per HTTP request (no
  batching), so a full LoCoMo run issues roughly one call per indexed message,
  plus one per query;
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
- one image is not cheap on a multimodal endpoint. Measured on Chat
  Completions with the OpenAI provider this document used to declare: a 96×96
  PNG billed 8,528 prompt tokens against 28 for the same prompt without it
  (8,500 tokens for one thumbnail). Ark embeds one image per request as well.
  Ingest images because the vector lane needs them, not because the context
  window is large.

The deployment declares the logical name `doubao-embedding-vision` and no
endpoint pin: the gateway serves that undated name, where Ark also offers the
dated snapshots (`doubao-embedding-vision-251215`, `-250615`) and account
endpoint IDs (`ep-xxx`). Point `bytedance`'s `base_url` at Ark and the same
document asks Ark instead. Without a vector lane at all, drop `embed:` and the
`bytedance` provider.

## Run

Ad-hoc lexical run (no model, recent + BM25 + entity lanes only):

```bash
go run ./cmd/memory-eval -samples 1
```

Full run through a deploy document with a real provider:

```bash
go run ./cmd/memory-eval -deploy ./deploy.yaml -env-file ../../../.env -samples 1
```

Useful flags: `-loader` (`locomo` | `longmemeval`, see below), `-samples N`
(conversations), `-questions N` (per conversation), `-max-items` /
`-max-tokens` (context budget), `-recent-items` (assembly `recent.max_items`),
`-out report.json`, `-build-only` (verify the deployment document builds and
wires without calling a model).

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

## LongMemEval

`-loader longmemeval` converts `xiaowu0162/longmemeval-cleaned`
(`longmemeval_oracle.json`, or `longmemeval_s_cleaned.json` for the hard
variant). Both files stay outside the repo — they are 15 MB and 277 MB, and the
`m` variant is 2.7 GB:

```bash
go run ./cmd/memory-eval -loader longmemeval -dataset /path/to/longmemeval_oracle.json \
  -deploy ./deploy.yaml -env-file ./.env -samples 0 -prepare-all -out lme.json
```

One instance is one scenario: `haystack_sessions` are its turns in date order,
and its single question is the graded one. On the oracle file that is 500
scenarios, 948 committed sessions and 10,960 messages.

Four conversion decisions a report depends on:

| | conversion |
| --- | --- |
| session date | `haystack_dates[i]` reaches every message of session `i` as the text prefix `[2023/05/30 (Tue) 23:40] …`, or as the `observed_at` metadata anchor under `-no-date-prefix -observed-at metadata`. One of the two is required: without it no relative-time question ("last year") resolves. The `dataset:` line reports `observed_at_anchored` / `observed_at_unparsed`, so coverage is a reading rather than an assumption. |
| question date | `question_date` → `AskedAt`, rendered into the answer prompt as `The question was asked at: …`. The dataset's own string is kept verbatim: the judge compares dates loosely, and reformatting would make "approximately right" unexplainable. |
| categories | `question_type` → 101 multi-session, 102 temporal-reasoning, 103 knowledge-update, 104 single-session-user, 105 single-session-assistant, 106 single-session-preference, 107 for a type this build does not know; the label is carried next to the number and printed by the `category` lines. The ids start above LoCoMo's on purpose: category 2 routes the reference harness's temporal suffix, so a LongMemEval question must never land on it. `-categories` accepts an id or a label. |
| evidence | the gold is session-level (`answer_session_ids`). A gold session counts as recalled when **any** of its messages was surfaced — the definition the dataset's own papers report; requiring every message would make a hit a function of how long the gold session is. `Turn.DatasetIDs` still names every message (`<session id>#<position>`), so a recalled item is matched back to its session, and the `session recall:` line reports that separately from the turn-level `evidence recall:` one. |

The judge is `-judge-style lme`, the default for this loader: the gold is a full
clause rather than a topic phrase, and two of the six types are not fact
questions. The style dispatches on the question's own labels —

- a fact question is graded on the gold answer (contains it, is equivalent to
  it, or states the intermediate steps; a subset is incorrect),
- `single-session-preference` (30 of 500) on the preferences the gold states:
  naming the same subject while ignoring the preference is incorrect,
- the 30 `_abs` rows on the refusal. Their gold says the history does not
  contain the answer, so returning a plausible fact is the failure being
  measured, not a near miss.

`-judge-style both` on this loader grades each answer twice, with `lme` and with
`locomo`, and writes both verdicts in one report: the `answer_*` fields and the
`lenient_*` ones, plus a `lenient_hits` / `lenient_rate` column on every
`category` line. That pair is the point of the mode — the strict half has to be
the rubric the dataset's own gold was written for, because pairing `strict` with
`locomo` would score all 30 preference rows and all 30 `_abs` rows as misses in
the first column. A `both` run therefore costs two judge calls per question; the
`usage` block lists them as the `judge` and `lenient_judge` roles.

Two consequences for reading a LongMemEval report next to a LoCoMo one:

- **The answer rate is not the published LongMemEval number.** That number is
  produced by the dataset's own judge prompt and reported with session-level
  recall; this harness answers from a recalled pack with the product policy
  (`-answer-style long`), so it measures the same store, not the same protocol.
- **`hit_rate` means something else here.** It is the fraction of questions
  whose gold sessions were all recalled, not the fraction whose gold turn was.
  Evidence granularity, conversation shape (948 committed sessions over 500
  instances, median 23 messages each) and the judge protocol all differ, so the
  two datasets' rates belong in two tables.

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
are not comparable with each other (the product prompt carries the long-form
rules and a 24k-rune context).

- **Short shape: +8.57pp for 3.0×.** Paired, 152 questions improved to 20
  regressed, McNemar p=2.6e-26; every category moved the same way.
- **Product shape: nothing for 2.6×.** Paired over the 1529 questions the two
  runs share, −0.26pp, 26 improved to 30 regressed, p=0.69. Open-domain is the
  one category that moved up (+4.2pp on n=96, inside its noise band) and
  multi-hop the one that moved down; the answer side still went 779 → 2034
  output tokens per question, measured by a paired 30-question cost probe over
  the same library (the same probe prices short at 601 and evidence at 1798).
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

### The answer model: flash, with pro behind a flag

`deepseek-v4-pro` is the same gateway's second generate model and the only
alternative `deploy.yaml` declares. Measured on one conversation's 152
questions, reading the same frozen library (`-skip-derive`, `library` block
identical in both runs: `conversations=2 underived=0`, evidence recall 84.21% in
both), product shape, strict judge, 20 items / 4096 tokens:

| | strict (answer) | cat 1 | cat 3 | answer out tokens | mean latency | answer stage |
| --- | --- | --- | --- | --- | --- | --- |
| `deepseek-flash` | 73.68% (112/152) | 43.8% | 69.2% | 65,979 | 503 ms | 3.5 min |
| `deepseek-v4-pro` | 73.03% (111/152) | 40.6% | 76.9% | 122,963 (1.9×) | 497 ms | 9.1 min |

`-1` question, `+1.9×` output tokens, `2.6×` the wall clock: on this
conversation the alternatives are inside the ±1-question/scenario band this
README quotes elsewhere, and the one column that moved (open-domain, n=13,
+7.7pp) is inside that sample's noise. So `deepseek-flash` stays the default
and `-answer-model deepseek-v4-pro` is the flag that prices the alternative on
a fuller run; both rows are strict-judged, and the stored full-corpus report is
where a lenient figure comes from.

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

A dataset that labels its gold by session instead of by turn (LongMemEval) is
graded by the same matcher at that granularity: a gold session is recalled when
any of its messages was surfaced, and the report carries `session recall` apart
from the turn-level figure rather than adding the two. See *LongMemEval* below.

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

- `-answer-provider` / `-answer-model` (default `gateway`/`deepseek-flash`:
  every report stored before this one names the provider `deepseek`, so the
  fingerprint's `answer_model` reads `gateway/deepseek-flash` only from here on
  and a `-baseline` compare across that rename reads as a configuration
  difference, which is what it is. `deepseek-v4-pro` is the declared
  alternative, text-only, so an `-images native` run cannot select it; a name
  neither declares answers 403 rather than being substituted --
  `TestConfiguredModelsExistInCatalog` checks the declaration against the
  catalog before a run starts)
- `-judge-provider` / `-judge-model` (default: the answering model)
- `-judge-style strict|locomo|lme|both|none` — `strict` asks for a bare
  `CORRECT` / `INCORRECT` token, `locomo` mirrors the leaderboard prompt
  (`{"correct":…}`), `lme` is LongMemEval's per-type rubric (and this loader's
  default), `none` runs the answering stage with no judge at all so the official
  token-F1 can be scored offline. `both` grades every answer twice: the strict
  half is the dataset's own rubric (`lme` on the `longmemeval` loader, `strict`
  on LoCoMo), the lenient half is always `locomo`. Only those verdict shapes are
  accepted; hedged prose is **ungraded** rather than guessed at, and ungraded
  answers leave the denominator (reported as `ungraded_answers`).
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
  The pass also reclaims derived storage nothing serves -- the lane bases and
  segments a compaction or a rebuild superseded, and the superseded document
  builds -- and prints what it removed, which is also how a run confirms that
  the workspace it wrote holds its live state rather than its history.
- `-reclaim`: sweep every scope the workspace's catalog holds and exit, with no
  dataset, no ingest, no derivation, and no pass. It is the same lane sweep
  `-maintain` runs, on its own: it lists each lane's object keys, deletes the
  bases and segments the active manifests do not name, and prints what each
  scope and each lane removed. What it removes is state no manifest can find,
  so it is safe on a workspace a report was measured on -- it does not judge
  facts, so it cannot change the overlay or the profile block a run serves.
  It needs `-deploy` (the document names the workspace, and the embedding model
  that names the vector lane's storage), and it reports objects rather than
  bytes: a sweep deletes by key, and measuring what it removes would read the
  storage it exists to give up.
- `-rebuild-lanes <names>`: re-index the named projection lanes of every scope
  the workspace's catalog holds and exit, with no dataset, no ingest, no
  derivation, and no model call of its own. It is what carries a workspace
  across a change to a lane's producer, and across a change to the form of what
  a lane stores: a lane's state lives under a storage name that covers both, so
  a build with a new form reads a name nothing was ever written under and leaves
  the generation an earlier build filled beside it, unread. A repair re-indexes
  canonical state -- the messages, the facts of the active generation, the
  chunks of the active document builds -- through the shapes the derivation
  worker writes, so nothing is derived again: watermarks and the published
  generation stay where they are, and the next pass resumes where it stopped.
  The value names lanes (`bm25`, `entity`, `vector`); an empty value rebuilds
  every lane whose rebuild needs no inference, and the vector lane is the one
  that embeds every artifact again, which is why it is named rather than
  rebuilt by default. It needs `-deploy`, and it prints per lane and per scope
  what was indexed, whether the state it replaced was another producer's
  (`stale_before`), and the digests of the state that replaced it.

### Reproducibility

Every run prints and stores a fingerprint, and `-baseline` warns when the
baseline was measured under a different configuration — comparing across
fingerprints compares two experiments, not a regression. Build with
`-buildvcs=true` (`go build -buildvcs=true -o memory-eval ./cmd/memory-eval`)
so the revision is stamped into the binary; without it the runner asks `git`
directly.

`deploy.yaml` is a template: the gateway's `deepseek-flash` (responses surface)
for fact extraction and its `doubao-embedding-vision` (Ark wire) for the vector
lane, plus `deepseek-v4-pro` and `azure-decision` declared for the answer A/B
and for the arbiter. `deploy-arbiter.yaml` is the same deployment with
`maintain.arbiter` on, and it is what both arbiter lanes build. See "Reading the
supersede rules" for what those lanes read, and "Diagnosing one question" for
the retrieval-side diagnostic.

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

The vector lane serves by an exact scan: it materializes the table of each shard
the request names, embeds the query, walks every entry a filter admits, and
sorts what it scored.
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
  itself (`latency`, and the shard tables it had to materialize: `load`,
  `shards`) and whether the store had them decoded (`decodes`) or paid to
  decode them (`resident+`, the payload they added to what the store keeps);
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
- the **resident state**: the tables the lane's store kept decoded, the payload
  they are reckoned at, and the store's own account of the reads — `hits` found
  decoded, `decodes` materialized off storage, `evictions` dropped to stay
  within the budget, `uncached` read and dropped because one table alone
  exceeded it — with the process's heap and RSS before and after.

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
kind and capability. `shard_errors` counts the lookups that returned an error,
apart from the loads the lane served.

## Reading the supersede rules

`memory-relation-zh` measures the two rules a maintenance pass ships with,
against a Chinese corpus, without a model and without a workspace:

```bash
go run ./cmd/memory-relation-zh          # human-readable
go run ./cmd/memory-relation-zh -json    # for a diff between two builds
```

It classifies a 31-phrase inventory of Chinese predicates against
`maintain.Vocabulary` (the same fold the pass uses, exported for this), scores
three negative phrases per class, and applies the similarity rule at
`maintain.DefaultSimilarityThreshold` over 13 two-turn scenarios — ten updates
and three pairs that can hold at once. The English control pairs are there to
show the table is not broken. What it reads at the time of writing: 0/31
predicates classified, 0/8 negative predicates misclassified, and 0/10 updates
above the threshold (the highest is 0.292). It measures the rules, not the
extractor: a predicate the model never writes is a predicate the rule never
sees.

The live halves answer the rest, and both need `GATEWAY_API_KEY`: they build
`deploy-arbiter.yaml`, whose one provider serves the extraction model, the
embedding model, and the decision surface the arbiter asks.

```bash
MEMORY_EVAL_LIVE=1 go test ./cmd/memory-relation-zh/ -run TestLiveArbiter -v
MEMORY_EVAL_LIVE=1 go test ./cmd/memory-eval/ -run TestArbiterJudges -v
```

`TestLiveArbiterOnTheRelationCorpus` scores the maintenance arbiter against the
same 13 labelled pairs: 10/10 updates called replaced, 0/3 controls, 3332
tokens for the batch (about 256 a pair) on `azure-decision`, the decision
surface the gateway serves. `TestArbiterJudgesDerivedChineseTurns` is the whole
path — the deployment's own extractor writes the facts, the pass proposes the
pairs, the arbiter judges them, and the overlay is what the pass wrote: on two
conversations it proposes both pairs, finds the update at 1.00, and leaves the
control current. It asserts the finding's reason is the arbiter, which is the
same statement as neither shipped rule deciding the pair.

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
| `TestExtractionModelAB` | is a candidate generate model good enough to extract? (failures, facts/turn, surface coverage, unsupported facts). Both arms are gateway reasoning models over 30 turns, so it runs far past `go test`'s 10m default: pass `-timeout` above the probe target's 60m |
| `TestAnswerContextSize` | how much context reaches the answer prompt |
| `TestEvidenceFidelity` | how much of the recalled evidence is raw text vs a fact paraphrase |
| `TestRetrievalBudgetSweep` | what evidence coverage each item budget buys |
| `TestPackingRedundancy` | what the packed items actually are: how many distinct commits the slots cover and how many carry the question's evidence |
| `TestDeriveConcurrencyLiveMatchesSequential` | does `derive.concurrency` change what is derived |
| `TestCoIngestedConversationsDoNotChangeRetrieval` | does the two-phase schedule change recall |
| `TestRejudgeStoredAnswersWithLenientJudge` | re-grade a stored report with the other judge style |
| `TestIngestTagsDatasetTurns` | does ingest tag every committed message with its dataset turn, and does a re-ingest repair a store that predates that tagging |
| `TestArbiterJudgesDerivedChineseTurns` | does the maintenance arbiter reach real derived Chinese facts: are the pairs proposed, is the update found, is the control left current |

The three retrieval probes read the same workspace the runner does and match
evidence the way the report does, so they need one that carries dataset turn ids
and they fail with the reason rather than report an untagged workspace as missed
recall (`requireTurnIDs`). `TestIngestTagsDatasetTurns` is the exception: it
ingests a conversation into a workspace of its own, because that is the only way
to get a store carrying ids out of a suite that must not derive a second
conversation.

Credentials resolve from `eval/.env` or the repository root; `MEMORY_EVAL_REPORT`
selects the report the re-judge probe reads.
