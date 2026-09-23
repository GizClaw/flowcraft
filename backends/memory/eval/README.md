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

Fill the key the deployment needs in the repository `.env` (git-ignored):

```bash
cp .env.example ../../../.env   # then edit ../../../.env
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
  turn's image (8-way parallel, 5s timeout, bounded cache), verifies it by magic
  bytes and inlines it as an `ImagePart`. The picture reaches the vector lane
  (the embedder takes text *and* image parts); the fact extractor and the answer
  prompt build their requests from `Content.Text()`, so they see the turn's text
  only. A download that fails falls back to the caption annotation, and
  `images_attached` / `images_failed` are printed. `-images annotation` keeps
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

## Two grading modes

The default run grades **evidence recall**, the metric the LoCoMo protocol
reports: the loader keeps each question's `evidence` turn ids, and a question
counts as a hit when every turn that carries the answer was surfaced. A turn is
surfaced either when a recalled item contains its committed text (raw message
items) or when the item's provenance resolves to it (a fact points at the
messages it was derived from — the runner wires that through the message
store). Results therefore carry both numbers: turn-level `evidence recall` and
`questions_with_full_evidence`.

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
| `TestAnswerContextSize` | how much context reaches the answer prompt |
| `TestEvidenceFidelity` | how much of the recalled evidence is raw text vs a fact paraphrase |
| `TestRetrievalBudgetSweep` | what evidence coverage each item budget buys |
| `TestPackingRedundancy` | what the packed items actually are |
| `TestDeriveConcurrencyLiveMatchesSequential` | does `derive.concurrency` change what is derived |
| `TestCoIngestedConversationsDoNotChangeRetrieval` | does the two-phase schedule change recall |
| `TestRejudgeStoredAnswersWithLenientJudge` | re-grade a stored report with the other judge style |

Credentials resolve from `eval/.env` or the repository root; `MEMORY_EVAL_REPORT`
selects the report the re-judge probe reads.
