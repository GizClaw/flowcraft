# memory eval

Evaluation module for `backends/memory`. It owns the whole eval surface:

- the `eval` package (scenario model, LoCoMo/LongMemEval loaders, `Run` /
  `RunAll`, baseline comparison), and
- the runners under `cmd/`: `memory-eval` (dataset run, optionally against a
  live inference provider through a deploy document) and `memory-eval-diag`
  (per-question retrieval diagnostics).

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
surfaced either when a recalled item contains its committed text (raw message
items) or when the item's provenance resolves to it (a fact points at the
messages it was derived from — the runner wires that through the message
store). Results therefore carry both numbers: turn-level `evidence recall` and
`questions_with_full_evidence`.

Ingest also tags every committed message with the dataset turn it was loaded
from (`DatasetTurnMetadataKey`), and that tag decides the match first: an item
whose sources name the turn counts, whatever the text says. Text alone is not
enough for a turn that carries an image, because the loader and the store
disagree about it — the loader renders `[shared image: caption]` into the turn
text, while a store ingested with `-images native` attaches the image as a part
and keeps the caption out of the text it holds. Measured on the workspace this
was built against: 231 of 7500 packed items (all raw recent turns) matched their
turn only after the caption was stripped, and every one of them was an evidence
turn a text-only match had to miss.

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

### Reproducibility

Every run prints and stores a fingerprint, and `-baseline` warns when the
baseline was measured under a different configuration — comparing across
fingerprints compares two experiments, not a regression. Build with
`-buildvcs=true` (`go build -buildvcs=true -o memory-eval ./cmd/memory-eval`)
so the revision is stamped into the binary; without it the runner asks `git`
directly.

`deploy.yaml` is a template: DeepSeek (responses surface) for fact extraction
and ByteDance Ark `doubao-embedding-vision` for the vector lane. `diag/` is a
small diagnostic that prints per-question source classes and top long-term
hits:

```bash
go run ./cmd/memory-eval-diag -questions 4
```

## Module dependency

`backends/memory` is not tagged yet, so `go.mod` pins it with a local
`replace ..`. When the backend is released, bump the require to the released
version and drop the replace (the `examples/forge` shape).

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
| `TestPackingRedundancy` | what the packed items actually are |
| `TestDeriveConcurrencyLiveMatchesSequential` | does `derive.concurrency` change what is derived |
| `TestCoIngestedConversationsDoNotChangeRetrieval` | does the two-phase schedule change recall |
| `TestRejudgeStoredAnswersWithLenientJudge` | re-grade a stored report with the other judge style |

Credentials resolve from `eval/.env` or the repository root; `MEMORY_EVAL_REPORT`
selects the report the re-judge probe reads.
