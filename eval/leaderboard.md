# FlowCraft eval — comparative leaderboard

> **Status: methodology draft (Phase 0).** No numbers published yet.
> This document records _how_ we will compare FlowCraft against
> same-direction open-source projects before any cross-framework
> result is committed to the repo. Phase 1 fills the result tables
> from published sources only (Approach B below); no adapter work
> is required. The methodology disclosures in
> `eval/README.md#methodology-disclosures` apply to every row.

## Why this is hard

Headline numbers from any memory / retrieval / agent harness encode
**at least five free variables** beyond "framework quality":

1. **Model under test** — LoCoMo qa.judge on Qwen-Max vs. GPT-4o
   diverges by 10-20pp regardless of framework.
2. **Judge LLM + judge prompt** — lenient mem0 prompt scores ~3-5pp
   higher than strict semantic-equivalence on the same predictions.
3. **Scope isolation** — pooling all LoCoMo conversations under one
   `user_id` cuts qa.judge by ~4× (see disclosure §A).
4. **Dataset version** — LongMemEval has had cleaning passes
   (xiaowu0162/longmemeval-cleaned); LoCoMo numbers depend on which
   commit of the upstream repo was converted.
5. **Cost ceiling** — a system that always invokes a reranker at
   recall time looks better but pays 5-10× the latency / tokens.
   Quality alone is not the whole story.

A leaderboard that ignores these is fast to publish and dishonest.
Every row in this document therefore ships with a **methodology
sub-row** that declares the five variables above plus the source
URL. A row missing any of them is "preview" and is excluded from
the headline table.

> **Freshness check before quoting.** Memory-system numbers move fast
> in 2026 (mem0 alone jumped LoCoMo +20 pp in 6 months, v2 → v3). Any
> row in this file dated more than ~6 months stale must be
> double-checked against the upstream project's latest blog / release
> notes / open-source `memory-benchmarks` repo before being cited
> externally. When a row's `Source` link points to a paper but the
> project's docs / blog have moved on (the mem0 paper vs the mem0 v3
> blog is the canonical example), surface the **newer** number with
> the older one kept as a historical line — never silently overwrite,
> never silently let the older number stand.

## Approach

Three approaches; **Phase 0-1 uses B exclusively. C / A are
deferred until we have a reason to invest.**

| Approach                      | What it is                                                                                                                                  | Cost                | Fairness                                                                   | Phase                    |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- | ------------------- | -------------------------------------------------------------------------- | ------------------------ |
| **B: cite published numbers** | Copy each competitor's published table from their paper / repo / blog post; link the source; declare the methodology gap for each row.      | hours per row       | acceptable **iff** the gap is annotated                                    | **Phase 0-1** (now)      |
| C: cite + selective our-rerun | B plus: where the competitor publishes a reproducible script, re-run their script on our infra with the same LLM and add a parallel column. | days per row        | high for the rerun column                                                  | Phase 2                  |
| A: full adapter               | Wire each competitor's SDK into `eval/runners/`; run their framework through our dataset + metrics.                                         | weeks per framework | highest, but only if the competitor's "default config" is matched honestly | Phase 3, gated on demand |

**Why B is enough for now.** Adapter work has poor ROI in the
pre-launch phase: it's expensive to write, fragile across competitor
releases, and contestable ("you didn't tune their config the way
they would"). A well-annotated citation table is faster, less
contestable, and gives external readers a real picture of where we
sit — every competitor we'd cite has already done the hard work of
publishing their own number, so we are not throwing away signal by
not re-running them.

**Phase-1 deliverable**: this `leaderboard.md` with every cell of
the four result tables either filled in from a public source (with
URL) or marked `N/A — no public number`. No code, no adapters, no
infrastructure work. Just sourcing.

## Per-direction competitor inventory

For each direction we list the **system under test (SUT)**, the
**variables to record per row** (so readers can tell which numbers
are comparable), and the **public-source candidates** Phase 1 will
cite. Adapter work, if it ever happens, lives in Phase 3.

### Direction 1: Long-term dialog memory (LoCoMo / LongMemEval)

**SUT**: the memory framework (Save / Recall API around an LLM).

**Per-row methodology to record**:

- model under test (answer-LLM the framework was driven with);
- judge prompt / judge model;
- scope-isolation policy (per-user vs. shared pool);
- dataset commit hash (LoCoMo: snap-research/locomo upstream commit;
  LongMemEval: cleaned vs. original dump).

**Public-source candidates** for Phase-1 citations:

- **mem0** — two distinct sets of numbers as of 2026:
  - **v2 (April 2025)**: the [mem0 paper](https://arxiv.org/abs/2504.19413)
    + their [github readme](https://github.com/mem0ai/mem0); LoCoMo 66.88,
    LongMemEval 67.8. Historical baseline.
  - **v3 (April 2026)**: the [token-efficient algorithm blog](https://mem0.ai/blog/mem0-the-token-efficient-memory-algorithm)
    + [memory-benchmarks](https://github.com/mem0ai/memory-benchmarks); LoCoMo
    91.6, LongMemEval 93.4, BEAM 64.1@1M / 48.6@10M. Single-pass
    ADD-only extraction + entity linking + 3-signal (semantic +
    lemmatized BM25 + entity-graph) retrieval; this is the current
    SOTA reference and what new memory systems should be measured
    against.
- **Letta** (formerly MemGPT) — [letta-ai/letta](https://github.com/letta-ai/letta)
  - the MemGPT [paper](https://arxiv.org/abs/2310.08560). Check
    whether they have a current LoCoMo number; if not, mark `N/A`.
- **Zep** — [getzep/zep](https://github.com/getzep/zep) +
  [blog comparisons](https://blog.getzep.com/). Production memory
  store with public benchmarks.
- **MemoryScope / ReMe** (Alibaba) — was
  [modelscope/MemoryScope](https://github.com/modelscope/MemoryScope),
  rebranded mid-2026 to [agentscope-ai/ReMe](https://github.com/agentscope-ai/ReMe)
  under the AgentScope umbrella. Heavyweight multi-worker pipeline
  (20+ workers: info filter, observation extractor, time extractor,
  contradiction-repeat filter, …), four memory layers (Personal /
  Task / Tool / Working). Self-claims LoCoMo 86.23; harder to
  reproduce apples-to-apples than mem0's single-pipeline
  architecture, but does set the high water mark for "academic
  pipeline" style memory systems.
- **A-MEM** — agentic memory, open source; locate the canonical repo
  before citing.
- **Cognee** — [topoteretes/cognee](https://github.com/topoteretes/cognee).
- **LongMemEval baseline tables** — the LongMemEval
  [paper](https://arxiv.org/abs/2410.10813) itself benchmarks several
  memory systems; useful "for context" rows.

**Closed-source caveats**: OpenAI Memory and Anthropic Memory have
no published LoCoMo / LongMemEval numbers. Don't speculate — mark
those cells `N/A — closed framework, no published number`.

### Direction 2: History compression (`eval history`)

**SUT**: a compactor strategy (none / buffer / compacted) inside the
SDK's history layer.

**Per-row methodology to record**:

- answer-LLM, summary-LLM, judge-LLM;
- compression budget (tokens);
- the underlying dataset (in our case LoCoMo10; competitors usually
  cite different ones, declare the source dataset per row).

**Public-source candidates**:

- **LLMLingua / LLMLingua-2** — [microsoft/LLMLingua](https://github.com/microsoft/LLMLingua)
  - papers; publishes compression-ratio vs. downstream-quality
    curves on LongBench / NaturalQuestions. Closest publicly-graded
    competitor for "shrink history, keep quality" strategy.
- **AutoCompressor** — Princeton, [paper](https://arxiv.org/abs/2305.14788).
- **GPTCache** — [zilliztech/gptcache](https://github.com/zilliztech/gptcache);
  query-cache, mostly orthogonal but worth a row for context.

**Caveat**: most prompt-compression work is benchmarked on
single-turn QA datasets (LongBench, MS-MARCO) rather than
multi-session dialog (LoCoMo). Direct comparison requires either
a re-run or an explicit "different dataset" warning per row.

### Direction 3: Knowledge retrieval (`eval knowledge`, `eval beir`)

**SUT**: the retrieval engine (BM25 + vector + hybrid lanes).

**Per-row methodology to record**:

- embedder model (vector lane);
- BEIR dataset + version;
- TopK / cutoffs.

**Public-source candidates** — BEIR is the easiest direction to
cite because the [official BEIR leaderboard](https://github.com/beir-cellar/beir)
already publishes nDCG@10 and Recall@100 per dataset for many
systems:

- **BM25 (Anserini / Lucene)** — the canonical BM25 baseline on the
  BEIR leaderboard. Cite directly for the `bm25` lane.
- **BGE-M3** — [FlagOpen/FlagEmbedding](https://github.com/FlagOpen/FlagEmbedding);
  current-SOTA-class dense embedder with public BEIR numbers.
- **E5** — [microsoft/unilm](https://github.com/microsoft/unilm/tree/master/e5)
  family, Microsoft, public BEIR numbers.
- **Cohere embed / OpenAI embed** — paid; cite their blog posts
  where numbers exist.

**Caveat**: our in-process BM25 (`sdk/retrieval/memory`) is a Go
reimplementation. A `bm25` cell that beats Anserini is suspicious;
one that loses by < 5% is the expected outcome. Phase-2 (selective
rerun) would settle this by spinning up Anserini on the same
corpus.

### Direction 4: SimpleQA factuality (`eval simpleqa`)

**SUT**: the **model**, not the framework. SimpleQA's protocol fixes
the answer prompt to literally `{question}` and the judge prompt to
OpenAI's official grader. The harness is supposed to be transparent
— if we rank "frameworks" here we're misusing the benchmark.

**What Phase-1 ships here**: a **per-model citation table** sourced
from each vendor's public blog post / release notes, e.g.:

- **OpenAI** — [SimpleQA announcement](https://openai.com/index/introducing-simpleqa/)
  has gpt-4o / o1 numbers.
- **Anthropic** — Claude 3.5 / 3.7 / 4 model cards.
- **Alibaba (Qwen)** — Qwen-Max / Qwen3 announcement posts.
- **DeepSeek** — DeepSeek-V3.x / R1 model cards.
- **xAI Grok**, **Mistral**, **Llama 3** — wherever public.

**Phase-1 self-check** (recommended): pick one publicly-graded model
(e.g. `gpt-4o-mini`), run our harness, and verify attempted-accuracy
is within ±1pp of the vendor's published number. If not, our
harness has drifted from the upstream protocol — fix it before
publishing anything else from this suite.

### Direction 5: Tool-use agents (`eval taubench`)

**SUT**: the agent harness (tool-call loop + state-validator)
**plus** the underlying LLM. These are not separable — different
harnesses make different turn-budget / planning decisions.

**Per-row methodology to record**:

- agent-LLM (and customer-LLM for multi-turn);
- domain pack (retail / airline / all);
- max-agent-turns, max-conversation-turns.

**Public-source candidates**:

- **Sierra's upstream τ-bench** — [sierra-research/tau-bench](https://github.com/sierra-research/tau-bench)
  - [paper](https://arxiv.org/abs/2406.12045). Their README publishes
    Pass@1 / Pass@4 for gpt-4o / Claude / Qwen / Llama. This is the
    canonical reference; cite their numbers directly.
- **τ²-bench** (Sierra's follow-up) — same handle on GitHub.
- **AgentBench**, **ToolBench**, **AutoGen** — too divergent in
  tool definitions to compare; out of scope for citation.

**Caveat**: τ-bench scores are tied to a specific tool-call schema
**and** the exact task set. The leaderboard rows below cite Sierra's
115-task retail / 50-task airline test sets, so the only fair
FlowCraft baseline is `eval-taubench.yml --domain sierra-retail` /
`--domain sierra-airline`, which loads Sierra's official `tasks_test.py`
and `data/*.json` (see `eval/taubench/sierra/README.md` for the
one-time staging on the runner). The `--domain retail | airline`
mini packs (5+7 hand-curated tasks) are harness smoke checks only —
their pass-rate is **not** the leaderboard number.

## Reproduction protocol

Every leaderboard row that represents **a FlowCraft number we ran
ourselves** (as opposed to a cited competitor number) must satisfy:

1. **Commit hash recorded** — the FlowCraft commit hash used.
2. **`.env` profile recorded** — the `FLOWCRAFT_<ALIAS>` JSON used
   (keys redacted) is committed under
   `eval/leaderboard/profiles/<row-id>.env.json` so the next operator
   can reproduce the model / endpoint combination.
3. **Report artifact archived** — the `${run_id}.json` report from
   the GitHub Actions run lives under
   `/var/lib/flowcraft-eval/reports/` on the runner host; the
   workflow run URL is in the methodology sub-row.
4. **Disclosure block ticked** — an explicit YAML block declares
   the §A-§E settings:

   ```yaml
   scope_isolation: per_conversation # §A
   em_definition: loose # §B
   extractor_prompt: tuned # §C
   judge_style: locomo # §D
   soft_merge: false # §E (publishing convention)
   ```

For **cited competitor rows**, points 1-3 are replaced by a single
source URL pointing at the paper / readme / blog post the number
came from; the YAML block records what the source declares (or
`unknown` for any setting the source did not specify).

A row missing any of these is filed as "preview" and excluded from
the headline table.

## Result tables

> All rows below are **cited from public sources** (Approach B).
> FlowCraft self-rows (marked `[pending]`) will be filled once we
> have a clean `--soft-merge=false` run on a comparable answer-LLM.
> Numbers are reproduced verbatim from their source; methodology
> gaps (judge model, answer-LLM, dataset version, scope policy) are
> declared next to each value. **Compare rows only when the
> declared variables match.**

### LoCoMo — overall qa.judge (LLM-as-judge)

> **LoCoMo has no official leaderboard.** Every paper that publishes
> a LoCoMo number runs the benchmark themselves under their own
> protocol (judge model, scope policy, version of the snap-research
> dataset, handling of the broken category-5). Cross-system numbers
> therefore disagree by 10-50 percentage points for the same system.
> We list **all available sources** per system and tag every row
> with its provenance — never collapse them into a single ranking.

#### Source A — Mem0 paper, Table 1 (verbatim)

These are the numbers Mem0 published in
[arXiv 2504.19413](https://arxiv.org/abs/2504.19413). They have been
disputed by Zep for the Zep row (see Source B); we keep them here
because they are still the most-cited single-source LoCoMo table.
Reproduced verbatim from
[memodb-io/memobase locomo-benchmark README](https://github.com/memodb-io/memobase/blob/main/docs/experiments/locomo-benchmark/README.md)
which copy-pasted the table from the mem0 paper.

> **⚠️ Source A is now historical.** As of April 2026 Mem0 has
> superseded these paper numbers with a redesigned algorithm —
> see Source A' below. The 66.88 / 68.44 figures correspond to the
> deprecated 2-pass ADD+UPDATE+DELETE pipeline. Use them only when
> comparing against systems that were themselves evaluated against
> this 2-pass Mem0 baseline (e.g. Zep's "75.14 corrected" row was
> measured against Mem0 v2; the meaningful question now is "how do
> you do against Mem0 v3" — see Source A').

| System                                    | Single-hop | Multi-hop | Open-domain |  Temporal | Overall (J%) |
| ----------------------------------------- | ---------: | --------: | ----------: | --------: | -----------: |
| OpenAI built-in memory                    |      63.79 |     42.92 |       62.29 |     21.71 |        52.90 |
| LangMem                                   |      62.23 |     47.92 |       71.12 |     23.43 |        58.10 |
| Zep ⚠️ (Mem0's eval, **disputed by Zep**) |      61.70 |     41.35 |       76.60 |     49.31 |        65.99 |
| Mem0                                      |  **67.13** |     51.15 |       72.93 |     55.51 |        66.88 |
| Mem0-Graph                                |      65.71 |     47.19 |       75.71 | **58.13** |    **68.44** |

**Methodology**:

```yaml
answer_llm: gpt-4o-mini
judge_model: gpt-4o-mini
dataset: snap-research/locomo v1 (10 conversations, ~200 q each, category 5 excluded)
source: mem0 paper Table 1 (April 2025), via memobase mirror
```

⚠️ **Zep dispute** ([Zep blog "Is Mem0 really
SOTA?"](https://blog.getzep.com/lies-damn-lies-statistics-is-mem0-really-sota-in-agent-memory/)):
Zep claims Mem0's 65.99 row used an incorrect Zep configuration
(wrong user model, timestamps appended to message bodies instead of
the `created_at` field, sequential vs. parallel search). Zep's own
correct implementation, evaluated on the same benchmark and reported
on Memobase's repo issue tracker, scores **75.14** ± 0.17 — see
Source B.

#### Source A' — Mem0 v3 token-efficient algorithm (April 2026)

In April 2026 Mem0 released a re-architected memory pipeline
([blog](https://mem0.ai/blog/mem0-the-token-efficient-memory-algorithm),
[v2→v3 migration docs](https://docs.mem0.ai/migration/oss-v2-to-v3),
[memory-benchmarks](https://github.com/mem0ai/memory-benchmarks))
that replaces the Source-A 2-pass extraction with **single-pass
ADD-only extraction + entity linking + 3-signal retrieval (semantic
+ lemmatized BM25 + entity-graph, rank-fused)**. The published
numbers are a step-change from the paper's Source-A rows:

| Category    | v2 (Source A) | v3 (Apr 2026) |   Delta |
| ----------- | ------------: | ------------: | ------: |
| Overall     |         71.4¹ |     **91.6** | **+20.2** |
| Single-hop  |          76.6 |          92.3 |   +15.7 |
| Multi-hop   |          70.2 |          93.3 |   +23.1 |
| Open-domain |          57.3 |          76.0 |   +18.7 |
| Temporal    |          63.2 |          92.8 |   +29.6 |

¹ v2 overall reported as 71.4 in the v3 blog; the 66.88 / 68.44 in
Source A are the **non-graph** / **graph** breakdowns from the
paper's Table 1, and the 71.4 reflects Mem0's pooled v2 number used
as the comparison baseline in the v3 blog. We keep both rows in the
table so the discrepancy is visible rather than papered over.

**Methodology (per the v3 blog)**:

```yaml
answer_llm: gpt-4o-mini   # single-pass retrieval, no agentic loops
judge_model: gpt-4o-mini
dataset: snap-research/locomo v1 (10 conversations)
mean_tokens_per_query: 6,956   # under 7k token budget
confidence: ±1 pp (judge inconsistency)
```

The v3 numbers are Mem0's managed-platform scores ("includes
proprietary optimizations not available in the open-source SDK").
Open-source v3 is expected to land "directionally similar" but not
identical — exact OSS numbers TBA. The open-source pipeline
([mem0ai/mem0](https://github.com/mem0ai/mem0)) is the right
reference implementation for anyone rebuilding the architecture
from primitives.

**Why this matters for FlowCraft**: our current `sdk/recall` pipeline
already does ADD-only extraction and assistant-fact ingestion (✓
parity with mem0 v3 on those two axes), but **lacks the entity
linking layer and lemmatized-keyword fusion** that drove most of
mem0's +20 pp. Any FlowCraft LoCoMo row that doesn't close those
gaps will land in the 75-80 band (Zep-corrected / Memobase-37
neighbourhood), not the 90+ band — see "What Phase 1 ships" for the
entity-layer + lemmatization work-tickets.

#### Source B — Zep's corrected number + Memobase's own rows

Same LoCoMo10 protocol as Source A (answer-LLM `gpt-4o-mini`, judge
`gpt-4o-mini`), but Zep's correct adapter and two Memobase versions.
From the
[memobase locomo-benchmark README](https://github.com/memodb-io/memobase/blob/main/docs/experiments/locomo-benchmark/README.md)
and [memobase issue #101](https://github.com/memodb-io/memobase/issues/101).

| System                       | Single-hop | Multi-hop | Open-domain |  Temporal | Overall (J%) |
| ---------------------------- | ---------: | --------: | ----------: | --------: | -----------: |
| Zep\* (Zep's corrected eval) |      74.11 |     66.04 |       67.71 | **79.79** |    **75.14** |
| Memobase v0.0.32             |      63.83 | **52.08** |       71.82 |     80.37 |        70.91 |
| Memobase v0.0.37             |  **70.92** |     46.88 |   **77.17** |     85.05 |    **75.78** |

Zep has subsequently announced (Dec 2025) an unverified
[80% LoCoMo at <200ms latency](https://blog.getzep.com/the-retrieval-tradeoff-what-50-experiments-taught-us-about-context-engineering/);
we'll cite that once the artifact lands in `getzep/zep-papers`.

#### Source C — MemEval third-party benchmark (ProsusAI)

Independent harness from ProsusAI that runs **9 memory systems
under one common LLM + embedder + judge** and reports both token-F1
and LLM-judge alongside total tokens. The most defensible
apples-to-apples LoCoMo comparison published so far, but the
absolute J numbers are **not directly comparable to Source A/B**
because the judge model is different (gpt-5.2 vs gpt-4o-mini).
From [ProsusAI/MemEval README](https://github.com/ProsusAI/MemEval).

| Rank | System              |        F1 |     Judge |   Tokens |
| :--: | ------------------- | --------: | --------: | -------: |
|  1   | PropMem (their own) | **0.605** | **0.823** |     5.9M |
|  2   | OpenClaw            |     0.557 |     0.725 |    16.4M |
|  3   | Full Context        |     0.542 |     0.709 |    37.5M |
|  4   | Hindsight           |     0.489 |     0.676 |    24.2M |
|  5   | Graphiti ⚠          |     0.416 |     0.573 |     5.1M |
|  6   | Memory-R1           |     0.389 |     0.569 |     3.4M |
|  7   | SimpleMem           |     0.358 |     0.478 |    11.4M |
|  8   | Mem0 ⚠              |     0.344 |     0.497 | **3.0M** |
|  9   | MemU ⚠              |     0.299 |     0.399 |     6.7M |

**Methodology**:

```yaml
answer_llm: gpt-4.1-mini
embedder: text-embedding-3-small
judge_model: gpt-5.2 (avg of relevance, completeness, accuracy)
dataset: LoCoMo10, 1986 QA pairs
```

⚠ MemEval's own fairness disclosures (worth quoting verbatim):

- **Graphiti row uses `graphiti-core` open-source library, not Zep's
  commercial platform** which runs Neo4j + BGE-M3 reranking. So the
  0.573 row is the OSS-graph engine alone, not the Zep-platform
  number from Sources A/B.
- **Mem0** had a known timestamp-handling bug ([mem0ai/mem0
  #3944](https://github.com/mem0ai/mem0/issues/3944)) at eval time;
  Mem0's temporal F1 collapsed to 0.104 here vs. 0.489 in the mem0
  paper.
- **MemU's "92% accuracy" headline uses LLM-judge binary accuracy on
  a different protocol** and is "not directly comparable to token
  F1". MemEval ranks MemU last at 0.399 J.

#### Source D — each system's own self-published number (canonical claims)

Each row below is what the project claims **on its own
README / paper / landing page**. These are the canonical citations
for "what the maintainer believes their system scores"; methodology
varies wildly. **Do not put these on the same ranking.**

| System                           |                                     Self-claimed LoCoMo | Methodology declared by maintainer                                           | Source                                                                                                                                                                                                                     |
| -------------------------------- | ------------------------------------------------------: | ---------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| OpenAI built-in memory           |                                                52.90 J% | answer + judge = gpt-4o-mini                                                 | [Mem0 paper](https://arxiv.org/abs/2504.19413) (only public source)                                                                                                                                                        |
| Mem0 / Mem0-Graph                |                                    66.88 / **68.44** J% | answer + judge = gpt-4o-mini                                                 | [Mem0 paper](https://arxiv.org/abs/2504.19413), Table 1 — **historical** (April 2025); see next row                                                                                                                       |
| **Mem0 v3 (April 2026 algorithm, managed platform)** |                              **91.6** J% | answer + judge = gpt-4o-mini, single-pass ADD + entity-link + 3-signal       | [Mem0 v3 blog](https://mem0.ai/blog/mem0-the-token-efficient-memory-algorithm) (April 16, 2026); ±1 pp judge variance; **~7k tokens / query** budget                                                                       |
| Zep                              |               **75.14** J% (corrected from earlier 84%) | answer = gpt-4o-mini, judge = gpt-4o-mini, parallel search                   | [Zep blog correction](https://blog.getzep.com/lies-damn-lies-statistics-is-mem0-really-sota-in-agent-memory/) + [zep-papers repo](https://github.com/getzep/zep-papers/tree/main/kg_architecture_agent_memory/locomo_eval) |
| Zep (Dec 2025 claim, unverified) |                                                    ~80% | "at <200ms latency"                                                          | [Zep retrieval-tradeoff blog](https://blog.getzep.com/the-retrieval-tradeoff-what-50-experiments-taught-us-about-context-engineering/)                                                                                     |
| MemOS (MemTensor)                |                                            **75.80** J% | "+43.70% vs OpenAI Memory"                                                   | [MemTensor/MemOS README](https://github.com/MemTensor/MemOS) badge                                                                                                                                                         |
| MemoryOS (BAI-LAB)               |      F1 +49.11% / BLEU +46.18% relative — no absolute J | gpt-4o-mini baseline                                                         | [BAI-LAB/MemoryOS README](https://github.com/BAI-LAB/MemoryOS)                                                                                                                                                             |
| MemU                             |                           **92.09%** "average accuracy" | unspecified; landing-page claim only                                         | [memu.pro/benchmark](https://memu.pro/benchmark)                                                                                                                                                                           |
| Memobase v0.0.37                 |                                            **75.78** J% | answer + judge = gpt-4o-mini, mem0-protocol                                  | [memobase locomo-benchmark README](https://github.com/memodb-io/memobase/blob/main/docs/experiments/locomo-benchmark/README.md)                                                                                            |
| MemoryScope / ReMe               |                                            **86.23** J% | answer-LLM "per table", judge = gpt-4o-mini                                  | [modelscope/MemoryScope README](https://github.com/modelscope/MemoryScope) (project rebranded mid-2026 to [agentscope-ai/ReMe](https://github.com/agentscope-ai/ReMe))                                                      |
| MemR3                            | "+7.29% over RAG, +1.94% over Zep" relative on LoCoMo10 | gpt-4.1-mini backend                                                         | [MemR³ paper](https://arxiv.org/abs/2512.20237), [Leagein/memr3](https://github.com/Leagein/memr3)                                                                                                                         |
| **FlowCraft**                    |                                             `[pending]` | `--soft-merge=false`, `--answer-llm=azure_4o_mini --judge-llm=azure_4o_mini` | our `eval-locomo.yml` run — **no real LoCoMo10 run yet**; LongMemEvalS numbers from §404 are a different dataset and must not be back-filled into this cell                                                                |

#### Discrepancy callout (read this before quoting any single number)

The same system on the same benchmark, depending on who runs the
harness:

- **MemU**: 92.09 (self, marketing page) vs ~61 (MemoryScope's
  reproduction; not in tables A-C but consistent with their README)
  vs **0.399 J ≈ 39.9** (MemEval, third-party). Spread: **52pp**.
- **Zep**: 65.99 (Mem0's disputed eval) vs **75.14** (Zep's
  correction) vs ~80% (Zep's Dec-2025 unverified claim) vs 0.573 J
  ≈ 57.3 (MemEval, but on graphiti-core not commercial Zep).
- **Mem0**: 66.88 (own paper, v2) vs 0.497 J ≈ 49.7 (MemEval, with
  known timestamp bug) vs **91.6** (v3 algorithm, April 2026 blog).
  Spread: **42pp** — and the v3 jump is what every new memory
  system now has to beat.

This is why FlowCraft's leaderboard headline will be a **single
`--soft-merge=false` row tied to one commit hash, one report
artifact, one declared judge prompt**, and a paragraph naming the
competitor numbers we're quoting from above — not a single
synthesised ranking.

### LongMemEval — LongMemEvalS (~115k tokens / question)

> **Difficulty tier disclosure.** LongMemEval ships three difficulty
> tiers (see `eval/longmemeval/README.md` — "Three flavors, three
> runtimes"): `_oracle` (evidence sessions only, <5k tok/Q, declared
> as "smoke / sanity check"), **`_s` (40 sessions, ~115k tok/Q, the
> "headline baseline" — this table)**, and `_m` (~500 sessions,
> ~1.5M tok/Q). The same memory stack typically drops 10-30 pp going
> from `_oracle` to `_s` because `_s` is the first tier where
> retrieval + ranking actually matter. **Do not compare an `_oracle`
> qa.judge to an `_s` qa.judge.** When citing FlowCraft's own
> numbers, always disclose which file the run was on.

| System                                    | answer-LLM  | Overall accuracy | Source                                                                                                                                                     |
| ----------------------------------------- | ----------- | ---------------: | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Llama 3.1 8B (long-context, no memory)    | self        |            45.4% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3b                                                                                               |
| Phi-3 14B (long-context, no memory)       | self        |            38.0% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3b                                                                                               |
| Llama 3.1 70B (long-context, no memory)   | self        |            33.4% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3b                                                                                               |
| Full-context with gpt-4o-mini (no memory) | gpt-4o-mini |            55.4% | [Zep blog](https://blog.getzep.com/state-of-the-art-agent-memory/)                                                                                         |
| GPT-4o (long-context, no memory)          | self        |            60.6% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3b                                                                                               |
| Mem0 v2 (independent eval, paper era)     | gpt-4o      |            49.0% | [Zep blog](https://blog.getzep.com/state-of-the-art-agent-memory/) (cited as "Mem0 independent evaluation"); deprecated by v3 below                       |
| Zep + gpt-4o-mini                         | gpt-4o-mini |            60.2% | [Zep blog](https://blog.getzep.com/state-of-the-art-agent-memory/)                                                                                         |
| Full-context with gpt-4o (no memory)      | gpt-4o      |            63.8% | [Zep blog](https://blog.getzep.com/state-of-the-art-agent-memory/)                                                                                         |
| Zep + gpt-4o                              | gpt-4o      |            71.2% | [Zep blog](https://blog.getzep.com/state-of-the-art-agent-memory/)                                                                                         |
| **Mem0 v3 (April 2026 algorithm, managed platform)** | gpt-4o-mini |     **93.4%** | [Mem0 v3 blog](https://mem0.ai/blog/mem0-the-token-efficient-memory-algorithm) (April 16, 2026); +25.6 pp vs v2's 67.8. Per-category: single-session-assistant 100.0 (+53.6), temporal 93.2 (+42.1), knowledge-update 96.2 (+16.7) |
| **FlowCraft**                             | gpt-4o-mini |      `[pending]` | our `eval-longmemeval.yml` run with `--answer-llm=azure_4o_mini --judge-llm=azure_4o_mini` + LongMemEval official grader; scheduled — see "What Phase 1 ships" §2 |

**Methodology common to cited rows above**:

```yaml
judge_model: gpt-4o (LongMemEval paper protocol)
judge_prompt: LongMemEval official grader
dataset: LongMemEvalS (115k tokens / question, ~50 sessions)
question_types: single-session-user/assistant/preference, multi-session, knowledge-update, temporal, abstention
```

The FlowCraft row stays `[pending]` until a parity run lands with
`--answer-llm=azure_4o_mini --judge-llm=azure_4o_mini` + the
LongMemEval official grader prompt (see "What Phase 1 ships" §2).
Any earlier FlowCraft LongMemEvalS numbers run with a
non-`gpt-4o-mini` answer-LLM are deliberately omitted from this
table — the parity-row publishing convention forbids mixing
answer-LLM provenance in a single row.

**Declared FlowCraft-row deltas vs cited rows** (kept lean, but
not zero):

1. **Judge model**: cited rows use `gpt-4o` as judge (LongMemEval
   paper protocol). FlowCraft uses `azure_4o_mini` for cost +
   alias-availability reasons. Same official grader **prompt**, so
   the prompt-style component of judge variance is held constant;
   only the model side moves. Per §1, judge-model-only variance on
   long-context QA is typically 2-5 pp.
2. **Answer-LLM deployment**: cited `gpt-4o-mini` is the OpenAI
   public API; FlowCraft's `azure_4o_mini` is the same model family
   served via Azure deployment. Documented public variance on
   identically-prompted tasks is ≤ 1 pp (cf. SimpleQA "FlowCraft
   parity check" below: `azure_4o_mini` returned 10.05% vs OpenAI
   cited 9.5% for the public deployment, well within ±1pp gate).

**Zep's per-question-type breakdown vs full-context baseline** (gpt-4o,
from [Zep blog](https://blog.getzep.com/state-of-the-art-agent-memory/)):

| Question type             | Full-context |   Zep |      Δ |
| ------------------------- | -----------: | ----: | -----: |
| single-session-preference |        20.0% | 56.7% |  +184% |
| single-session-assistant  |        94.6% | 80.4% | −17.7% |
| single-session-user       |        81.4% | 92.9% | +14.1% |
| temporal-reasoning        |        45.1% | 62.4% | +38.4% |
| multi-session             |        44.3% | 57.9% | +30.7% |
| knowledge-update          |        78.2% | 83.3% |  +6.5% |

This breakdown matters because it shows a memory system that crushes
full-context on temporal / multi-session / preference reasoning
_loses_ on the simplest single-session-assistant questions (the
condensation step drops information that full-context still has).
A leaderboard with only "overall" hides this trade-off.

### LongMemEval — commercial systems (short history pilot)

[LongMemEval paper](https://arxiv.org/abs/2410.10813) §3.4 also evaluates
ChatGPT's built-in memory + Coze on a **97-question subset with
3–6 session histories** (~10× shorter than LongMemEvalS); these are
_not_ directly comparable to the S-scale rows above but useful for
context on where closed-source assistant memory sits today:

| System                    | Underlying LLM | Accuracy | Source                                                       |
| ------------------------- | -------------- | -------: | ------------------------------------------------------------ |
| Offline reading (oracle)  | gpt-4o         |   91.84% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3a |
| ChatGPT (built-in memory) | gpt-4o-mini    |   71.13% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3a |
| ChatGPT (built-in memory) | gpt-4o         |   57.73% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3a |
| Coze (built-in memory)    | gpt-4o         |   32.99% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3a |
| Coze (built-in memory)    | gpt-3.5-turbo  |   24.74% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3a |

Note the inversion: ChatGPT-with-gpt-4o-mini scores 13pp _higher_
than ChatGPT-with-gpt-4o. The paper attributes this to gpt-4o's
tendency to overwrite memory entries it deems "outdated".

### BEIR scifact

> **Sourcing note**: per-dataset BEIR breakdown is awkward. BGE-M3's
> paper and OpenAI's `text-embedding-3-large` announcement publish
> only **BEIR-average** or **MTEB-average**, not SciFact-specific
> nDCG@10. The verifiable per-task numbers below come from each
> model's **HuggingFace `model-index` YAML** (the canonical
> machine-readable claim shipped with each model card), with the
> BEIR-paper BM25 baseline as the lexical anchor. Models without a
> public `model-index` entry on SciFact are marked accordingly.

| Engine / model                  | Type                            |                    nDCG@10 |                 Recall@100 | Source                                                                                                                                                                                                                                                                      |
| ------------------------------- | ------------------------------- | -------------------------: | -------------------------: | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| BM25 (Anserini / Lucene)        | sparse / lexical                |                      0.665 |                          — | [BEIR paper](https://arxiv.org/pdf/2104.08663) Anserini reference; also cited by [Anserini regression docs](https://github.com/castorini/anserini/blob/master/docs/regressions-beir-v1.0.0-scifact-flat.md)                                                                 |
| BGE-large-en-v1.5 (English)     | dense                           |                 **0.7461** |                     0.9483 | [BAAI/bge-large-en-v1.5 HF model-index YAML](https://huggingface.co/BAAI/bge-large-en-v1.5/blob/main/README.md)                                                                                                                                                             |
| E5-large-v2 (English)           | dense                           |                 **0.7224** |                     0.9627 | [intfloat/e5-large-v2 HF model-index YAML](https://huggingface.co/intfloat/e5-large-v2/blob/main/README.md)                                                                                                                                                                 |
| multilingual-e5-large           | dense (multi-lingual)           |                 **0.7041** |                     0.9387 | [intfloat/multilingual-e5-large HF model-index YAML](https://huggingface.co/intfloat/multilingual-e5-large/blob/main/README.md)                                                                                                                                             |
| BGE-M3 (dense mode)             | dense (multi-lingual, long-doc) | `[not published per-task]` | `[not published per-task]` | [BGE-M3 paper](https://arxiv.org/abs/2402.03216) publishes BEIR-avg only; SciFact breakdown not in the [HF model card](https://huggingface.co/BAAI/bge-m3) (no `model-index` YAML). Pull from [MTEB leaderboard](https://huggingface.co/spaces/mteb/leaderboard) if needed. |
| OpenAI text-embedding-3-large   | dense                           | `[not published per-task]` | `[not published per-task]` | [OpenAI embedding blog](https://openai.com/index/new-embedding-models-and-api-updates/) only publishes MTEB-avg 64.6 and MIRACL-avg 54.9; no SciFact breakdown. Community-submitted MTEB results circulate but lack a canonical author-blessed citation.                    |
| **FlowCraft (doc-level BM25, `retrieval` backend)** | sparse / lexical, doc-level | **0.6725** | **0.9076** | run [`25851783774`](https://github.com/GizClaw/flowcraft/actions/runs/25851783774); `--lanes bm25 --root /var/lib/flowcraft-eval/datasets/scifact --ingest_concurrency 8` (sdk v0.3.14) |
| **FlowCraft (vector / hybrid)** | dense / hybrid                  |                `[pending]` |                `[pending]` | same, with `--embedder qwen:text-embedding-v4`                                                                                                                                                                                                                              |

**Reading the FlowCraft row**: 0.6725 nDCG@10 / 0.9076 Recall@100
matches Anserini's Lucene BM25 baseline (0.665 nDCG@10) within
noise — both implementations score against doc-level BM25 corpus
stats (DocCount = 5,183 logical docs, AvgLength = avg doc length).
We get there via the `sdk/knowledge/backend/retrieval` adapter
backed by `sdk/retrieval/memory`, which since
[#143](https://github.com/GizClaw/flowcraft/pull/143) maintains a
dedicated `__docs` namespace alongside the per-chunk namespace
inside the same `retrieval.Index` and routes `SearchDocuments`
queries through it. MRR was 0.6352, errors = 0, ingest wall-clock
~10 min @ `ingest_concurrency=8`.

**Why this row is new** — earlier results on this row (`0.180`
nDCG@10 from run [`25839108340`](https://github.com/GizClaw/flowcraft/actions/runs/25839108340)
and the chunks→docID `max` / `sum` / `first` aggregation ablation)
used the `fs` backend's chunk-level Search path with adapter-side
collapse. That path is mathematically incapable of recovering
doc-level BM25 (the BM25 IDF term `log((N − df + 0.5) / (df + 0.5) + 1)`
is highly nonlinear in `N`, and sum/max-pooling chunk-level scores
cannot recover the doc-level signal — see
[#126](https://github.com/GizClaw/flowcraft/issues/126) for the
math). [#134](https://github.com/GizClaw/flowcraft/issues/134)
chartered the fix; the now-merged
[#127](https://github.com/GizClaw/flowcraft/pull/127) and
[#143](https://github.com/GizClaw/flowcraft/pull/143) deliver it.

**Backend rotation log** — keep this table when you're reproducing
or comparing historical numbers; the `retrieval` row above is the
current canonical FlowCraft BM25 number on scifact.

| backend / config                                                                              | run                                                                            | nDCG@10    | Recall@100 | wall-clock | note                                                                                            |
| --------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------ | ---------: | ---------: | ---------: | ----------------------------------------------------------------------------------------------- |
| **`retrieval` (doc-level via #143 `__docs` namespace, current)**                              | [`25851783774`](https://github.com/GizClaw/flowcraft/actions/runs/25851783774) | **0.6725** | **0.9076** | ~13.5 min  | this is the leaderboard row; matches Anserini Lucene within noise                              |
| `fs` (doc-level via #127 bespoke per-dataset doc-level inverted index)                        | [`25844184454`](https://github.com/GizClaw/flowcraft/actions/runs/25844184454) | 0.6725     | 0.9076     | ~30 min    | identical numbers, 2× slower ingest; FSChunkRepo is deprecated for v0.5.0 removal               |
| `retrieval` (chunk-overfetch + sum-pool, [#137](https://github.com/GizClaw/flowcraft/pull/137) original design) | [`25848699992`](https://github.com/GizClaw/flowcraft/actions/runs/25848699992) | 0.1325     | 0.7266     | ~14 min    | failed #134 acceptance (5× degradation); root cause = chunk-level corpus stats, fixed by #143   |
| `fs` chunk-level Search + adapter-side `max`-pool aggregation (historical)                    | [`25839108340`](https://github.com/GizClaw/flowcraft/actions/runs/25839108340) | 0.180      | 0.255      | —          | original incorrect path before #126/#127/#134 work; kept for archaeology only                   |
| `fs` chunk-level Search + adapter-side `sum`-pool aggregation (ablation)                      | [`25840471567`](https://github.com/GizClaw/flowcraft/actions/runs/25840471567) | 0.054      | 0.207      | —          | length-biased on scifact (long non-rel docs accumulate per-chunk noise); same archaeology       |

**Methodology**: all rows use the standard BEIR scifact split
(corpus.jsonl + queries.jsonl + qrels/test.tsv, 5,183 docs, 300 test
queries). BM25 numbers from BEIR are Anserini's Lucene
implementation with default parameters; FlowCraft's in-process Go
BM25 (`sdk/textsearch`, k1=1.2, b=0.75, CJKTokenizer) now scores
within ±0.01 of that baseline once it sees the same doc-level
corpus statistics, confirming the implementation is correct and
the historical ~0.5-nDCG gap was purely a granularity-mismatch
artefact.

### τ-bench — retail (Pass@1)

| Strategy / agent                    |      Pass@1 |      Pass@4 | Source                                                                    |
| ----------------------------------- | ----------: | ----------: | ------------------------------------------------------------------------- |
| TC + gpt-4o-mini                    |        (??) |        (??) | [Sierra README](https://github.com/sierra-research/tau-bench) (marked ??) |
| TC + claude-3-5-sonnet-20240620     |       0.626 |       0.387 | [Sierra README](https://github.com/sierra-research/tau-bench)             |
| TC + gpt-4o                         |       0.604 |       0.383 | [Sierra README](https://github.com/sierra-research/tau-bench)             |
| TC + claude-3-5-sonnet-20241022     |   **0.692** |   **0.462** | [Sierra README](https://github.com/sierra-research/tau-bench)             |
| **FlowCraft (TC + same agent-LLM)** | `[pending]` | `[pending]` | our `eval-taubench.yml --domain sierra-retail` (Sierra's 115-task `tasks_test.py`; staging in `eval/taubench/sierra/README.md`) |

### τ-bench — airline (Pass@1)

| Strategy / agent                    |      Pass@1 |      Pass@4 | Source                                                        |
| ----------------------------------- | ----------: | ----------: | ------------------------------------------------------------- |
| TC + gpt-4o-mini                    |       0.225 |       0.100 | [Sierra README](https://github.com/sierra-research/tau-bench) |
| TC + claude-3-5-sonnet-20240620     |       0.360 |       0.139 | [Sierra README](https://github.com/sierra-research/tau-bench) |
| TC + gpt-4o                         |       0.420 |       0.200 | [Sierra README](https://github.com/sierra-research/tau-bench) |
| TC + claude-3-5-sonnet-20241022     |   **0.460** |   **0.225** | [Sierra README](https://github.com/sierra-research/tau-bench) |
| Act (gpt-4o)                        |       0.365 |       0.140 | [Sierra README](https://github.com/sierra-research/tau-bench) |
| ReAct (gpt-4o)                      |       0.325 |       0.160 | [Sierra README](https://github.com/sierra-research/tau-bench) |
| **FlowCraft (TC + same agent-LLM)** | `[pending]` | `[pending]` | our `eval-taubench.yml --domain sierra-airline` (Sierra's 50-task `tasks_test.py`; staging in `eval/taubench/sierra/README.md`) |

**Caveat**: Sierra's main README warns that the original retail /
airline task definitions are outdated; they recommend
[τ³-bench](https://github.com/sierra-research/tau2-bench) as the
current canonical version. Our mini-fixtures track the original
schema. When we publish, link both.

### SimpleQA — per-model (not framework)

SimpleQA scores attribute almost entirely to the model under test,
not to the harness around it. This table is therefore a
**model citation reference**, not a framework comparison. FlowCraft
is in the harness column; if our number for the same model differs
by more than ±1pp from the cited one, the harness has drifted from
the upstream protocol and we owe an investigation before publishing
anything else from this suite.

All rows are SimpleQA overall accuracy (zero-shot, chain-of-thought,
official grader) pulled from
[`openai/simple-evals` README](https://github.com/openai/simple-evals#benchmark-results)
— the top-level benchmark table, "SimpleQA" column. The non-OpenAI
rows are what `openai/simple-evals` itself cites from each vendor's
announcement post; we keep the second-hand citation rather than
re-deriving from the vendor card.

| Model                      | SimpleQA accuracy | Primary source                                                                                                                                                            |
| -------------------------- | ----------------: | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| o1-mini                    |               7.6 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| gpt-4.1-nano-2025-04-14    |               7.6 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| gpt-4o-mini-2024-07-18     |               9.5 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| o3-mini                    |              13.4 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| gpt-4.1-mini-2025-04-14    |              16.8 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| o4-mini                    |              20.2 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| Claude 3 Opus              |              23.5 | [Anthropic Claude 3 announcement](https://www.anthropic.com/news/claude-3-family) via [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)     |
| Claude 3.5 Sonnet          |              28.9 | [Anthropic Claude 3.5 announcement](https://www.anthropic.com/news/claude-3-5-sonnet) via [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results) |
| gpt-4o-2024-11-20          |              38.8 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| gpt-4o-2024-05-13          |              39.0 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| gpt-4o-2024-08-06          |              40.1 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| gpt-4.1-2025-04-14         |              41.6 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| o1-preview                 |              42.4 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| o1                         |              42.6 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| o3-high                    |              48.6 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| o3                         |              49.4 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |
| gpt-4.5-preview-2025-02-27 |          **62.5** | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                                                                                           |

**FlowCraft parity check** (Phase-1 self-check): pick
`gpt-4o-mini-2024-07-18` (cheapest publicly-graded model), run
`eval-simpleqa.yml --answer-llm <gpt-4o-mini alias>`, and verify
attempted-accuracy is within ±1pp of 9.5%. Status: **✓ PASSED** —
[run 25837614443](https://github.com/GizClaw/flowcraft/actions/runs/25837614443)
on `azure_4o_mini` (200Q smoke, judge=`azure_4o_mini`) returns
**attempted_accuracy=10.05%** (delta=+0.55 pp, well inside the
±1pp gate and inside the 1σ sampling error of ~2.1 pp at this
sample size); `judge_failures=0` confirms the official SimpleQA
grader prompt parses cleanly end-to-end. Implication: when this
same harness reports another model's attempted_accuracy, that
number is `openai/simple-evals`-equivalent and citeable on this
table directly.

**Qwen / DeepSeek / Grok 3 / Llama 3.1 SimpleQA numbers**: not in
the openai/simple-evals README. Cite from each vendor's
announcement post when filling in. Status: `[pending]`.

## What Phase 1 ships

A practical citation-only first cut, day-by-day rough order:

1. **LoCoMo10** — mem0 v2 paper row + **mem0 v3 (April 2026) blog
   row (currently the SOTA at 91.6)**; Letta row if any; Zep row from
   its blog; LongMemEval paper baselines as "for context". FlowCraft
   self-row deferred until the "entity-layer + lemmatized BM25"
   work lands (see §New work below) — current `sdk/recall` is
   single-pass ADD-only but lacks the entity-index + 3-signal
   fusion that drove mem0 v2 → v3's +20pp, so any "all gpt-4o-mini"
   FlowCraft row published today would land in the 56-80 band
   depending on extractor choice and be misleading to readers
   expecting current SOTA. _Half a day of sourcing once the
   FlowCraft side is ready._
2. **LongMemEval** — paper baselines + Zep blog rows filled (§404
   table). **Outstanding for the FlowCraft row**:
   1. parity run with `--answer-llm=azure_4o_mini --judge-llm=azure_4o_mini`
      (LongMemEval official grader prompt) on `longmemeval_s.jsonl`
      — Zep blog's `Zep + gpt-4o-mini = 60.2%` is the most directly
      comparable cited row;
   2. surface `Report.PerQuestion.Tags` so the §428 per-type table
      can carry a FlowCraft column;
   3. (optional) `_oracle` row for the same stack to anchor the
      `_oracle → _s` drop curve. _Half a day each._
3. **BEIR scifact** — pull BM25 (Anserini) + BGE-M3 + E5 numbers
   from the BEIR leaderboard. FlowCraft self-row from
   `eval-beir.yml`. _A few hours._
4. **SimpleQA self-check** — one `eval-simpleqa.yml` run on
   `gpt-4o-mini`; verify within ±1pp of OpenAI's announcement
   number; fix the harness if not. _A few hours._
5. **τ-bench retail** — pull Sierra's published Pass@1 for the same
   agent-LLMs from their readme. FlowCraft self-row from
   `eval-taubench.yml`. _A few hours._

After this lands, the table is comparable on inspection (everyone
can see the methodology gaps) and we have a credible "where do we
stand" picture without writing a line of adapter code.

### New work surfaced by the mem0 v3 (April 2026) release

The April-2026 mem0 v3 algorithm jumped LoCoMo from 71.4 → 91.6 by
adding two architectural primitives FlowCraft does not currently
have. Closing each is a discrete tracked work-item before any
FlowCraft LoCoMo row is published:

1. **Entity-index retrieval layer** — auto-extract entities (proper
   nouns, quoted strings, compound noun phrases) into a dedicated
   index alongside the chunk/dense indices, and have `pipeline.LTM`
   query all three in parallel with rank-fusion. Today `sdk/recall`
   carries an `entities` field on every fact but does **not**
   maintain a separate retrieval channel keyed by them. Expected
   contribution: +10~15 pp on multi-hop / open-domain LoCoMo
   categories (where entity overlap is the dominant signal).
2. **Lemmatized BM25** — `sdk/textsearch`'s tokenizer/stemmer needs
   a verb-form normalization pass so "attending a meeting" and
   "what meetings did I attend" share a key. mem0 v3 blog calls
   out this single change as "measurable impact" — expected +2~5 pp
   spread across the standard categories.
3. **(Tracking only)** BEAM benchmark (1M / 10M token scales,
   [mem0ai/memory-benchmarks](https://github.com/mem0ai/memory-benchmarks)
   contains the harness). New benchmark direction; we are not
   committing to ship a FlowCraft BEAM row in Phase 1 because the
   evaluation cost is significant and BEAM is solving a problem
   (10M token context retrieval) FlowCraft is not yet optimised
   for. Re-evaluate after the entity-layer + lemmatization land.

## Open questions for the operator

These do not block Phase 0 (this methodology) but block Phase 1
(filling in numbers):

1. **Publishing cadence.** Quarterly? On every FlowCraft minor
   release? — affects how stale rows are handled.
2. **Reproduction budget.** FlowCraft self-rows cost LLM tokens. Do
   we pay for nightly regressions or only on demand?
3. **Where the leaderboard is hosted.** This `.md` file?
   `docs.flowcraft.dev/leaderboard`? Internal-only? — affects how
   careful we must be with the "this number could be misread"
   framing.
4. **Who signs off** on a new row. Eval owner alone, or a second
   reviewer? — same governance as a release.
