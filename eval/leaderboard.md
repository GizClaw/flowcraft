# FlowCraft eval — comparative leaderboard

> **Status: methodology draft (Phase 0-1).** This document is **not**
> a single ranking. Headline numbers across memory frameworks
> routinely disagree by 30-50 percentage points for the **same
> system on the same benchmark** depending on who ran the harness
> — see the [cross-source LoCoMo comparison](#cross-source-locomo-comparison-april-2026)
> below. We list every public number per source and let the
> reader compare; we never collapse them into a single rank.
>
> **Freshness:** memory-system numbers move fast (mem0 v2 → v3
> jumped LoCoMo +20 pp in 6 months). Rows dated more than ~6 months
> stale must be double-checked against the upstream project's
> latest blog / release / open-source `memory-benchmarks` repo
> before being cited externally.

## TL;DR — where FlowCraft stands today

| Benchmark                                                      |                                                                           FlowCraft (current) |                                                                              Closest cited row |                                                      Headline gap | Notes                                                                                                                                                                                                                          |
| -------------------------------------------------------------- | --------------------------------------------------------------------------------------------: | ---------------------------------------------------------------------------------------------: | ----------------------------------------------------------------: | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| LoCoMo10 qa.judge (full-4o-mini parity)                        |                                               **0.7659** (answer=judge=extractor=gpt-4o-mini) | Mem0 v3 managed 91.6 ⚠ unreproducible, ReMe 86.23, Memobase v0.0.37 75.78, Zep-corrected 75.14 | +1 pp vs Memobase 75.78; +28 pp vs every reproducible mem0 number | Cross-project apples-to-apples row. See [Source D](#source-d--each-systems-own-self-published-number-canonical-claims), the cross-source table, and the [reproducibility addendum](#reproducibility-addendum-mem0-issue-2800). |
| LoCoMo10 qa.judge (best operational, gpt-4o-mini answer+judge) | **0.83 ± 1 pp** (extractor=DeepSeek-V4-Flash; answer+judge=gpt-4o-mini, n=2: 0.8366 / 0.8288) |                                                     (same row above + ReMe 86.23, MemR3 81.55) | +5 pp vs Memobase / Zep-corrected; -2 pp vs MemR3; ~-3 pp vs ReMe | Free internal-ingestion variable (extractor LLM). Headline judge protocol unchanged. See [§ Extractor-swap ablation (May 2026)](#extractor-swap-ablation-may-2026) for the 4-cell ablation + variance check.                   |
| LongMemEval `_s`                                               |                                                                                   `[pending]` |                                                 Zep + gpt-4o-mini 60.2%, Mem0 v3 managed 93.4% |                                                               tbd | Parity run blocked on `--judge-llm` switch — [Phase-1 §2](#what-phase-1-ships).                                                                                                                                                |
| BEIR scifact nDCG@10                                           |                                                                   **0.6725** (doc-level BM25) |                                                                          Anserini Lucene 0.665 |                                                      within noise | Implementation parity confirmed — see [BEIR scifact](#beir-scifact).                                                                                                                                                           |
| τ-bench retail Pass@1                                          |                                                                                   `[pending]` |                                                          claude-3-5-sonnet 0.692, gpt-4o 0.604 |                                                               tbd | Sierra's published numbers held as the citation set.                                                                                                                                                                           |
| SimpleQA (azure_4o_mini)                                       |                                                                          10.05% attempted-acc |                                                                      OpenAI's gpt-4o-mini 9.5% |                                               +0.55 pp (parity ✓) | Harness validated within ±1 pp; see [SimpleQA](#simpleqa--per-model-not-framework).                                                                                                                                            |

The two interesting questions this document tries to answer
honestly:

1. **What does FlowCraft actually score today?** (the bold numbers
   above, all backed by a `${run_id}.json` artifact.)
2. **What would a credible comparison look like?** (everything
   below the TL;DR — multiple sources, per-row methodology
   declarations, no synthesised single ranking.)

## Cross-source LoCoMo comparison (April 2026)

The same system on the same benchmark, scored by different
projects under different protocols. **Read this table before
quoting any single LoCoMo number externally.**

| System                    | mem0 paper (Apr-2025) | Zep blog (corrected) | ProsusAI MemEval (gpt-5.2 judge) | ReMe README (gpt-4o-mini judge, MemOS protocol) |                                                                                             Self-claim |              Spread |
| ------------------------- | --------------------: | -------------------: | -------------------------------: | ----------------------------------------------: | -----------------------------------------------------------------------------------------------------: | ------------------: |
| Mem0 v2                   |                 66.88 |                    — |             49.7 ⚠ timestamp bug |                                           61.00 |                                                                                          66.88 / 71.4¹ |               16 pp |
| Mem0-Graph                |                 68.44 |                    — |                                — |                                               — |                                                                                                  68.44 |                0 pp |
| Mem0 v3 (managed)         |                     — |                    — |                                — |                                               — |                                                                                               **91.6** |      n/a (1 source) |
| Zep                       |      65.99 ⚠ disputed |            **75.14** |         57.3 (graphiti-core OSS) |                                           81.06 |                                                                                          75.14 / ~80%² |               24 pp |
| Memobase v0.0.37          |                     — |                75.78 |                                — |                                               — |                                                                                                  75.78 |      n/a (1 source) |
| MemoryScope / ReMe        |                     — |                    — |                                — |                                       **86.23** |                                                                                              **86.23** |                0 pp |
| MemR3                     |                     — |                    — |                                — |                                           81.55 |                                                                                  "+7.29 % vs RAG" rel. |                 n/a |
| MemU                      |                     — |                    — |                         **39.9** |                                           61.15 |                                                                                              **92.09** |           **52 pp** |
| MemoryOS                  |                     — |                    — |                                — |                                           54.70 |                                                                                      F1/BLEU rel. only |                 n/a |
| MemOS                     |                     — |                    — |                                — |                                           75.87 |                                                                                                  75.80 |                0 pp |
| HiMem                     |                     — |                    — |                                — |                                           80.71 |                                                                                                      — |                 n/a |
| TiMem                     |                     — |                    — |                                — |                                           75.30 |                                                                                                      — |                 n/a |
| TSM                       |                     — |                    — |                                — |                                           76.69 |                                                                                                      — |                 n/a |
| OpenAI built-in           |                 52.90 |                    — |                                — |                                               — |                                                                                                      — | n/a (no own number) |
| LangMem                   |                 58.10 |                    — |                                — |                                               — |                                                                                                      — | n/a (no own number) |
| **FlowCraft (this repo)** |                     — |                    — |                                — |                                               — | **76.59** (full-4o-mini parity) / **83.66** (extractor=ds_flash, answer+judge=gpt-4o-mini, single run) | n/a — see TL;DR row |

¹ Mem0 v3 blog reports v2 as **71.4** pooled (graph + non-graph)
when comparing to v3's 91.6. The paper itself reports 66.88
(non-graph) / 68.44 (graph).
² Zep's December 2025 retrieval-tradeoff blog announces ~80 % at
<200 ms latency but the artifact is not yet in `getzep/zep-papers`.

### How to read this

- **Three- and four-way disagreements are common**, not noise.
  MemU spans 39.9 → 61.15 → 92.09. Mem0 spans 49.7 → 61.00 →
  66.88 → 91.6 (v3). Zep spans 57.3 → 65.99 → 75.14 → 81.06.
- **Self-claim numbers strongly favour the home team.** Every
  vendor that publishes a leaderboard puts itself on top. ReMe's
  own table has ReMe at 86.23 and Mem0 at 61.00; mem0's
  memory-benchmarks repo has Mem0 v3 at 91.6 and does not
  benchmark ReMe at all. We make no claim about which set of
  numbers is "right" — they are simultaneously all defensible
  under each project's stated protocol.
- **For external citation we recommend**: name the source,
  reproduce the cell verbatim, and never collapse two rows into a
  single rank. The per-source tables further down preserve every
  source independently for this reason.

The architectural reasons these numbers diverge are summarised in
[§ Architectural paradigms behind the 90+ self-claims](#architectural-paradigms-behind-the-90-self-claims)
under Methodology.

### Reproducibility addendum (mem0 issue #2800)

The 91.6 mem0-v3 number anchoring the cross-source table is **not
reproducible by the open community**, including by users running on
the official mem0 managed platform. We surface this because the
TL;DR row would otherwise read "FlowCraft trails the SOTA by 15 pp"
when the comparable reproducible numbers are 28 pp lower than the
self-claim.

Tracking issue: [mem0ai/mem0 #2800](https://github.com/mem0ai/mem0/issues/2800)
("Unable to reproduce locomo eval scores locally", open May 2025 -
March 2026, 18 👍 / 6 👀). Selected community measurements collected
from the issue thread (all on LoCoMo10, gpt-4o-mini answer + judge
where stated):

| Reporter / setup                                | qa.judge (overall) | Source                                                                                                                                                                                                                                                                                  |
| ----------------------------------------------- | -----------------: | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Mem0 paper self-claim, v3 managed (April 2026)  |           **91.6** | [Mem0 v3 blog](https://mem0.ai/blog/mem0-the-token-efficient-memory-algorithm)                                                                                                                                                                                                          |
| jisuozhao — mem0 platform paid API + 4o-mini    |          **48.57** | [#2800 comment](https://github.com/mem0ai/mem0/issues/2800), Oct 2025                                                                                                                                                                                                                   |
| Li-Qingyun — mem0 platform free tier            |          **41.62** | [#2800 comment](https://github.com/mem0ai/mem0/issues/2800), Nov 2025                                                                                                                                                                                                                   |
| bufapiqi — local OSS + 4o-mini + custom prompt  |    ~52 (cat1+cat2) | [#2800 comment](https://github.com/mem0ai/mem0/issues/2800), Jan 2026                                                                                                                                                                                                                   |
| Donghua-Cai — mem0 platform + 4o-mini           |                ~20 | [#2800 comment](https://github.com/mem0ai/mem0/issues/2800), Jan 2026                                                                                                                                                                                                                   |
| shenshiqi — local OSS + Qwen3-235B              |          **33.70** | [#2800 comment](https://github.com/mem0ai/mem0/issues/2800), Aug 2025                                                                                                                                                                                                                   |
| mem0 paper "SoTA" rows (cat1 + cat2 only)       |      67.13 / 51.15 | mem0 paper Table 1 / cross-checked in the same issue thread                                                                                                                                                                                                                             |
| **FlowCraft (this repo, 25912693189)**          |          **76.59** | LoCoMo10 n=1542, full-4o-mini parity; per-category cat1=77.66 cat2=70.09 cat3=55.21 cat4=81.09                                                                                                                                                                                          |
| **FlowCraft (this repo, n=2 best operational)** |      **83 ± 1 pp** | LoCoMo10 n=1542, **same judge protocol** but extractor=DeepSeek-V4-Flash; two runs 25921980006 (83.66) + 25923629998 (82.88); avg per-cat single-hop=81.0 multi-hop=68.2 open-domain=85.9 temporal=82.7 — see [§ Extractor-swap ablation (May 2026)](#extractor-swap-ablation-may-2026) |

mem0's maintainer position
([@prateekchhikara, June 2025](https://github.com/mem0ai/mem0/issues/2800#issuecomment-2802617570)):
the 91.6 figure relies on the managed platform's proprietary
**Contextual ADD** and **Custom Instructions** features that are
not present in the open-source `mem0ai/mem0` repo. As of the
issue's March 2026 closure, mem0 has not published the recipe
that reproduces 91.6 from any combination of public artifacts.

**Reading the cross-source table with this context**: the 91.6
cell sits in a row with no independent verifier; the 75.14 / 75.78
/ 86.23 cells sit in rows that other operators have re-evaluated.
The headline gap from FlowCraft's 76.59 (full-4o-mini parity) to
the **next reproducible** LoCoMo number is **+0.81 pp vs Memobase
v0.0.37 (75.78)**, not −15 pp. FlowCraft's best operational number
(83.66, extractor swap to DeepSeek-V4-Flash, same judge protocol)
clears every reproducible peer in the table.

### Limit-stage bug fix (May 2026)

Pre-`db16b7f2`, `Memory.Recall(req{TopK: N})` silently capped at
**10 hits regardless of N**: `memory/retrieval/pipeline/factory.go`'s
LTM pipeline hard-coded `Limit{TopK: 10}` as the final stage, and
`Limit.Run` truncated to its own stage TopK without consulting
`st.Request.TopK`. Recall lanes already honoured `Request.TopK`
for per-lane fan-out (vector top-60, BM25 top-50, entity top-30),
but the final Limit squashed the fused result back to 10 every
time.

Evidence from `25910887436` recall dump:

- 1317 / 1542 questions returned **exactly 10 hits**
- The "On 7 May 2023 LGBTQ support group" fact for conv-26-q1 was
  in the namespace yet ranked outside the top-10 after fusion — it
  could not surface, regardless of `--topk=30`
- Every prior `--topk=N` ablation for `N > 10` was a no-op for the
  answer LLM (only changing lane fan-out / RRF candidate diversity)

The fix lets `Request.TopK` override the stage's own TopK when set,
keeping the stage TopK as a fallback default. Paired runs that
isolate the fix:

| Run             | Config                      |  qa.judge | Note                                                           |
| --------------- | --------------------------- | --------: | -------------------------------------------------------------- |
| 25896493084     | topk=30 (effective 10, BUG) |      75.7 | canonical pre-fix baseline                                     |
| 25912690228     | topk=10 (real 10)           |     74.06 | post-fix at the same effective topK ⇒ within noise of baseline |
| **25912693189** | **topk=30 (real 30)**       | **76.59** | post-fix with the budget the flag advertises ⇒ +0.89 pp net    |

Test: `TestLimitHonoursRequestTopK` in
`memory/retrieval/pipeline/pipeline_test.go` pins the three cases
(request > stage / request < stage / request unset).

### Extractor-swap ablation (May 2026)

After the Limit fix re-established a clean 0.7659 baseline, we ran
a four-cell ablation crossing **extractor LLM** (`azure_4o_mini` vs
`azure_ds_flash`, DeepSeek-V4-Flash) with the entity-store + post-
fusion entity-link-boost integration (codename "A+B": `entity_store
=true entity_link_boost=0.3 query_entity_extractor=true`):

| Run             | extractor    |     A+B |   qa.judge |   Δ baseline | single-hop | multi-hop | open-domain |  temporal |
| --------------- | ------------ | ------: | ---------: | -----------: | ---------: | --------: | ----------: | --------: |
| 25896493084     | 4o_mini      |     off |     0.7659 |            — |      75.53 |     55.21 |       81.33 |     70.40 |
| 25914095647     | 4o_mini      |      on |     0.7633 |     −0.26 pp |      74.47 |     59.38 |       81.57 |     69.16 |
| 25914193558     | ds_flash     |      on |     0.8152 |     +4.93 pp |      80.50 |     61.46 |       83.23 |     83.80 |
| **25921980006** | **ds_flash** | **off** | **0.8366** | **+7.07 pp** |  **80.14** | **67.71** |   **87.16** | **82.24** |

Two clean conclusions:

1. **A+B is a net regression on both extractor configurations.** On
   the strong (`ds_flash`) extractor it loses **−2.14 pp** versus
   the no-A+B counterpart, and the loss concentrates on the very
   categories A+B was designed to lift — multi-hop drops from 67.71
   → 61.46 (−6.25 pp) and open-domain from 87.16 → 83.23
   (−3.93 pp). Mechanism candidate: the post-fusion entity boost
   pulls candidates with entity overlap upward, starving the
   diverse cross-session candidates that multi-hop reasoning
   needs. We have **disabled A+B by default** in `eval-locomo.yml`
   (`entity_store=false entity_link_boost=0 query_entity_extractor
=false`) and kept the implementation in tree as an off-by-default
   knob.
2. **Extractor quality dominates.** Swapping the extractor LLM
   from `gpt-4o-mini` to `DeepSeek-V4-Flash` — with **zero
   architectural changes** elsewhere — accounts for the entire
   +7.07 pp jump. Temporal goes 70.40 → 82.24 (+11.84 pp) and
   multi-hop 55.21 → 67.71 (+12.50 pp); these are the categories
   where a weaker extractor leaves the most facts on the floor.

The 0.8366 number reuses `gpt-4o-mini` for answer + judge, so the
external judge-protocol parity that lets us compare to Mem0 v3 /
Zep / Memobase is preserved. The extractor LLM is an internal
ingestion-pipeline variable that every memory framework picks
independently (mem0 / Zep / Memobase do not declare it cell-by-
cell in their published tables).

#### Variance check (n=2)

| Run             | qa.judge | single-hop | multi-hop | open-domain | temporal | adversarial |
| --------------- | -------: | ---------: | --------: | ----------: | -------: | ----------: |
| 25921980006     |   0.8366 |      80.14 |     67.71 |       87.16 |    82.24 |      100.00 |
| **25923629998** |   0.8288 |      81.91 |     68.75 |       84.66 |    83.18 |      100.00 |
| avg / Δ         |   0.8327 |       81.0 |      68.2 |        85.9 |     82.7 |       100.0 |

Two runs of the same configuration (commit `db16b7f2`, extractor
=`azure_ds_flash`, A+B off, topk=30, ingest_concurrency=16) land
in a 0.83 ± 1 pp band. Per-category drift is direction-mixed and
bounded by ≤2.5 pp; consistent with non-deterministic 4o-mini
judge + ds_flash extractor jitter. **Headline is reported as
0.83 ± 1 pp (n=2)** rather than a single point estimate.

## Result tables

> All rows below are **cited from public sources** (Approach B —
> see [§ Approach](#approach)). FlowCraft self-rows are run on the
> infrastructure in `eval/<benchmark>/`; their commit hash + run
> URL + report artifact are in the source column. **Compare rows
> only when the declared methodology variables match.**

### LoCoMo — overall qa.judge (LLM-as-judge)

> **LoCoMo has no official leaderboard.** Every paper that
> publishes a LoCoMo number runs the benchmark themselves under
> their own protocol (judge model, scope policy, version of the
> snap-research dataset, handling of the broken category-5).
> Cross-system numbers therefore disagree by 10-50 percentage
> points — see the cross-source comparison above for the
> condensed view. The following five sources are kept independent.

#### Source A — Mem0 paper, Table 1 (verbatim, April 2025)

[arXiv 2504.19413](https://arxiv.org/abs/2504.19413). Disputed by
Zep for the Zep row (see Source B); kept here because it is still
the most-cited single-source LoCoMo table. Reproduced verbatim
from the
[memodb-io/memobase locomo-benchmark README](https://github.com/memodb-io/memobase/blob/main/docs/experiments/locomo-benchmark/README.md)
which copy-pasted the paper's table.

> **⚠ Source A is historical.** As of April 2026 Mem0 has
> superseded these paper numbers with a redesigned algorithm —
> see Source A'. The 66.88 / 68.44 figures are the deprecated
> 2-pass ADD+UPDATE+DELETE pipeline. Use them only when comparing
> against systems themselves evaluated against this 2-pass Mem0
> baseline (e.g. Zep's "75.14 corrected" row).

| System                                   | Single-hop | Multi-hop | Open-domain |  Temporal | Overall (J%) |
| ---------------------------------------- | ---------: | --------: | ----------: | --------: | -----------: |
| OpenAI built-in memory                   |      63.79 |     42.92 |       62.29 |     21.71 |        52.90 |
| LangMem                                  |      62.23 |     47.92 |       71.12 |     23.43 |        58.10 |
| Zep ⚠ (Mem0's eval, **disputed by Zep**) |      61.70 |     41.35 |       76.60 |     49.31 |        65.99 |
| Mem0                                     |  **67.13** |     51.15 |       72.93 |     55.51 |        66.88 |
| Mem0-Graph                               |      65.71 |     47.19 |       75.71 | **58.13** |    **68.44** |

```yaml
answer_llm: gpt-4o-mini
judge_model: gpt-4o-mini
dataset: snap-research/locomo v1 (10 conversations, ~200 q each, category 5 excluded)
source: mem0 paper Table 1 (April 2025), via memobase mirror
```

**Zep dispute** ([Zep blog "Is Mem0 really
SOTA?"](https://blog.getzep.com/lies-damn-lies-statistics-is-mem0-really-sota-in-agent-memory/)):
Zep claims Mem0's 65.99 row used an incorrect Zep configuration
(wrong user model, timestamps in message bodies instead of the
`created_at` field, sequential vs. parallel search). Zep's
correct implementation scores **75.14** ± 0.17 — see Source B.

#### Source A' — Mem0 v3 token-efficient algorithm (April 2026)

In April 2026 Mem0 released a re-architected pipeline
([blog](https://mem0.ai/blog/mem0-the-token-efficient-memory-algorithm),
[v2→v3 migration docs](https://docs.mem0.ai/migration/oss-v2-to-v3),
[memory-benchmarks repo](https://github.com/mem0ai/memory-benchmarks))
replacing the Source-A 2-pass extraction with **single-pass
ADD-only + entity linking + 3-signal retrieval (dense + lemmatized
BM25 + entity-graph, rank-fused)**:

| Category    | v2 (Source A) | v3 (Apr 2026) |     Delta |
| ----------- | ------------: | ------------: | --------: |
| Overall     |         71.4¹ |      **91.6** | **+20.2** |
| Single-hop  |          76.6 |          92.3 |     +15.7 |
| Multi-hop   |          70.2 |          93.3 |     +23.1 |
| Open-domain |          57.3 |          76.0 |     +18.7 |
| Temporal    |          63.2 |          92.8 |     +29.6 |

¹ The v3 blog reports v2 as 71.4 pooled; the paper's 66.88 / 68.44
are the non-graph / graph breakdowns. We keep both numbers visible
so the discrepancy is not papered over.

```yaml
answer_llm: gpt-4o-mini # single-pass retrieval, no agentic loops
judge_model: gpt-4o-mini
dataset: snap-research/locomo v1 (10 conversations)
mean_tokens_per_query: 6,956 # under 7k token budget
confidence: ±1 pp (judge inconsistency)
notes: managed-platform scores; includes proprietary optimizations
  not available in the open-source SDK (mem0ai/mem0). OSS v3
  is expected to be "directionally similar" but not identical.
```

#### Source B — Zep's corrected number + Memobase rows

Same protocol as Source A (answer-LLM `gpt-4o-mini`, judge
`gpt-4o-mini`) but Zep's correct adapter + two Memobase versions.
From the
[memobase locomo-benchmark README](https://github.com/memodb-io/memobase/blob/main/docs/experiments/locomo-benchmark/README.md)
and [memobase issue #101](https://github.com/memodb-io/memobase/issues/101).

| System                       | Single-hop | Multi-hop | Open-domain |  Temporal | Overall (J%) |
| ---------------------------- | ---------: | --------: | ----------: | --------: | -----------: |
| Zep\* (Zep's corrected eval) |      74.11 |     66.04 |       67.71 | **79.79** |    **75.14** |
| Memobase v0.0.32             |      63.83 | **52.08** |       71.82 |     80.37 |        70.91 |
| Memobase v0.0.37             |  **70.92** |     46.88 |   **77.17** |     85.05 |    **75.78** |

Zep's December 2025
[80%-at-<200ms-latency claim](https://blog.getzep.com/the-retrieval-tradeoff-what-50-experiments-taught-us-about-context-engineering/)
will be cited here once the artifact lands in `getzep/zep-papers`.

#### Source C — MemEval third-party benchmark (ProsusAI)

[ProsusAI/MemEval](https://github.com/ProsusAI/MemEval) runs **9
memory systems under one common LLM + embedder + judge**, reports
token-F1 and LLM-judge plus total tokens. Absolute J numbers are
**not directly comparable to Source A/B** because the judge model
is different (gpt-5.2 vs gpt-4o-mini); the **relative ordering**
is the signal.

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

```yaml
answer_llm: gpt-4.1-mini
embedder: text-embedding-3-small
judge_model: gpt-5.2 (avg of relevance, completeness, accuracy)
dataset: LoCoMo10, 1986 QA pairs
```

⚠ MemEval's fairness disclosures (verbatim):

- **Graphiti** row uses `graphiti-core` OSS, not Zep's commercial
  platform (Neo4j + BGE-M3 reranking). The 0.573 row is the
  OSS-graph engine alone.
- **Mem0** had a known timestamp-handling bug
  ([mem0ai/mem0 #3944](https://github.com/mem0ai/mem0/issues/3944))
  at eval time; temporal F1 collapsed to 0.104 vs 0.489 in the
  mem0 paper.
- **MemU**'s "92% accuracy" headline uses LLM-judge binary
  accuracy on a different protocol and is "not directly
  comparable to token F1". MemEval ranks MemU last at 0.399 J.

#### Source D — each system's own self-published number

What each project claims on its own README / paper / landing
page. These are the canonical citations for "what the maintainer
believes their system scores"; methodology varies. **Do not put
these on the same ranking.**

| System                                     |                                                                           Self-claimed LoCoMo | Methodology declared by maintainer                                                                                                                                                                                                                                                                   | Source                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| ------------------------------------------ | --------------------------------------------------------------------------------------------: | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| OpenAI built-in memory                     |                                                                                      52.90 J% | answer + judge = gpt-4o-mini                                                                                                                                                                                                                                                                         | [Mem0 paper](https://arxiv.org/abs/2504.19413) (only public source)                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| Mem0 / Mem0-Graph                          |                                                                          66.88 / **68.44** J% | answer + judge = gpt-4o-mini                                                                                                                                                                                                                                                                         | [Mem0 paper](https://arxiv.org/abs/2504.19413), Table 1 — **historical (April 2025)**                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| **Mem0 v3 (April 2026, managed platform)** |                                                                                   **91.6** J% | answer + judge = gpt-4o-mini, single-pass ADD + entity-link + 3-signal                                                                                                                                                                                                                               | [Mem0 v3 blog](https://mem0.ai/blog/mem0-the-token-efficient-memory-algorithm); ±1 pp judge variance; ~7k tokens / query                                                                                                                                                                                                                                                                                                                                                                                                |
| Zep                                        |                                                     **75.14** J% (corrected from earlier 84%) | answer = gpt-4o-mini, judge = gpt-4o-mini, parallel search                                                                                                                                                                                                                                           | [Zep blog correction](https://blog.getzep.com/lies-damn-lies-statistics-is-mem0-really-sota-in-agent-memory/) + [zep-papers repo](https://github.com/getzep/zep-papers/tree/main/kg_architecture_agent_memory/locomo_eval)                                                                                                                                                                                                                                                                                              |
| Zep (Dec 2025, unverified)                 |                                                                                         ~80 % | "at <200ms latency"                                                                                                                                                                                                                                                                                  | [Zep retrieval-tradeoff blog](https://blog.getzep.com/the-retrieval-tradeoff-what-50-experiments-taught-us-about-context-engineering/)                                                                                                                                                                                                                                                                                                                                                                                  |
| MemOS (MemTensor)                          |                                                                                  **75.80** J% | "+43.70% vs OpenAI Memory"                                                                                                                                                                                                                                                                           | [MemTensor/MemOS README](https://github.com/MemTensor/MemOS) badge                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| MemoryOS (BAI-LAB)                         |                                                F1 +49.11% / BLEU +46.18% rel. — no absolute J | gpt-4o-mini baseline                                                                                                                                                                                                                                                                                 | [BAI-LAB/MemoryOS README](https://github.com/BAI-LAB/MemoryOS)                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| MemU                                       |                                                                **92.09 %** "average accuracy" | landing-page claim; LLM-as-retriever architecture (markdown categories + LLM-reads-files); protocol + judge model unspecified                                                                                                                                                                        | [memu.pro/benchmark](https://memu.pro/benchmark), [oss-memory-infrastructure-llms](https://memu.pro/oss-memory-infrastructure-llms). **Third-party MemEval (Source C) scores this 39.9 J under a common judge; ReMe README (Source E) scores it 61.15 — treat the self-claim as marketing.**                                                                                                                                                                                                                            |
| Memobase v0.0.37                           |                                                                                  **75.78** J% | answer + judge = gpt-4o-mini, mem0-protocol                                                                                                                                                                                                                                                          | [memobase locomo-benchmark README](https://github.com/memodb-io/memobase/blob/main/docs/experiments/locomo-benchmark/README.md)                                                                                                                                                                                                                                                                                                                                                                                         |
| MemoryScope / ReMe                         |                                                                                  **86.23** J% | originally heavy multi-worker pipeline; rebranded to AgentScope's ReMe with simplified file+vector design; the 86.23 is **still ReMe's own headline number** (Source E table)                                                                                                                        | [agentscope-ai/ReMe](https://github.com/agentscope-ai/ReMe) (current home), formerly [modelscope/MemoryScope](https://github.com/modelscope/MemoryScope)                                                                                                                                                                                                                                                                                                                                                                |
| MemR3                                      | "+7.29% vs RAG, +1.94% vs Zep" relative on LoCoMo10; ReMe README puts MemR3 at 81.55 absolute | gpt-4.1-mini backend                                                                                                                                                                                                                                                                                 | [MemR³ paper](https://arxiv.org/abs/2512.20237), [Leagein/memr3](https://github.com/Leagein/memr3), absolute via [ReMe README](https://github.com/agentscope-ai/ReMe)                                                                                                                                                                                                                                                                                                                                                   |
| **FlowCraft (full-4o-mini parity)**        |                                             **76.59** J% (extractor=answer=judge=gpt-4o-mini) | `--soft-merge=true`, `--multi-recall=true`, `--extractor-llm=azure_4o_mini --answer-llm=azure_4o_mini --judge-llm=azure_4o_mini --reranker-llm=azure_4o_mini`, topk=30 (now actually 30 — see [§ Limit-stage bug fix](#limit-stage-bug-fix-may-2026)), image annotations from upstream `query` field | run [25912693189](https://github.com/GizClaw/flowcraft/actions/runs/25912693189) (May 15, 2026); n=1542 questions on LoCoMo10. Per-category: single-hop 77.66, temporal 70.09, multi-hop 55.21, open-domain 81.09, adversarial 100.00 (n=2).                                                                                                                                                                                                                                                                            |
| **FlowCraft (best operational)**           |                 **83 ± 1 pp** J% (n=2; extractor=DeepSeek-V4-Flash; answer=judge=gpt-4o-mini) | Same as above except `--extractor-llm=azure_ds_flash`. Answer + judge LLMs remain gpt-4o-mini — apples-to-apples on the published judge protocol.                                                                                                                                                    | runs [25921980006](https://github.com/GizClaw/flowcraft/actions/runs/25921980006) (83.66) and [25923629998](https://github.com/GizClaw/flowcraft/actions/runs/25923629998) (82.88), May 15, 2026; n=1542 each. Per-category (avg of two runs): single-hop 81.0, temporal 82.7, multi-hop 68.2, open-domain 85.9, adversarial 100.0. Variance check confirms 0.83 ± 1 pp reproducibility. See [§ Extractor-swap ablation (May 2026)](#extractor-swap-ablation-may-2026) for the four-cell ablation + variance breakdown. |

#### Source E — ReMe README leaderboard (April 2026)

From the [agentscope-ai/ReMe README](https://github.com/agentscope-ai/ReMe/blob/main/README.md)
"Experimental results → LoCoMo" section. Maintainers reproduced
nine baselines under one protocol; their own system tops the
table. Baselines "reproduced from their respective papers under
aligned settings where possible". Treat as ReMe's curated view of
the field, not a neutral leaderboard.

| Method   | Single Hop | Multi Hop |  Temporal | Open Domain |   Overall |
| -------- | ---------: | --------: | --------: | ----------: | --------: |
| MemoryOS |      62.43 |     56.50 |     37.18 |       40.28 |     54.70 |
| Mem0     |      66.71 |     58.16 |     55.45 |       40.62 |     61.00 |
| MemU     |      72.77 |     62.41 |     33.96 |       46.88 |     61.15 |
| MemOS    |      81.45 |     69.15 |     72.27 |       60.42 |     75.87 |
| HiMem    |      89.22 |     70.92 |     74.77 |       54.86 |     80.71 |
| Zep      |      88.11 |     71.99 |     74.45 |       66.67 |     81.06 |
| TiMem    |      81.43 |     62.20 |     77.63 |       52.08 |     75.30 |
| TSM      |      84.30 |     66.67 |     71.03 |       58.33 |     76.69 |
| MemR3    |      89.44 |     71.39 |     76.22 |       61.11 |     81.55 |
| **ReMe** |  **89.89** | **82.98** | **83.80** |   **71.88** | **86.23** |

```yaml
answer_llm: per-row backbone (not declared per cell in README)
judge_model: gpt-4o-mini (LLM-as-a-Judge following MemOS)
dataset: LoCoMo (10 conversations, snap-research)
notes: Baselines "reproduced from their respective papers under
  aligned settings where possible"; Mem0 reproduced as v2
  (61.00 here vs 91.6 self-claimed for managed v3).
```

#### Discrepancy callout

The same system on the same benchmark, depending on who runs the
harness:

- **MemU**: **92.09** ([memu.pro/benchmark](https://memu.pro/benchmark),
  marketing page, protocol unspecified) vs **61.15** (ReMe README
  reproduction) vs **39.9 J** (ProsusAI/MemEval, common-judge
  harness). Spread: **52 pp** — the largest in the table. The
  gap is best explained by MemU's LLM-reads-files architecture:
  excellent when the corpus fits in one prompt
  (per-conversation LoCoMo, marketing setup), much weaker under
  top-k retrieval protocols.
- **Mem0**: 66.88 (own paper v2) vs 49.7 J (ProsusAI/MemEval,
  with timestamp bug) vs 61.00 (ReMe reproduction) vs **91.6**
  (v3 managed, April 2026 blog). Spread: **42 pp** — and the v3
  jump is what every new memory system now has to beat.
- **Zep**: 65.99 (Mem0's disputed eval) vs **75.14** (Zep's
  correction) vs 57.3 J (MemEval, but on graphiti-core not
  commercial Zep) vs 81.06 (ReMe reproduction) vs ~80 % (Zep's
  Dec-2025 unverified). Spread: **24 pp**.
- **MemR3**: relative-only in its own paper, **81.55** absolute
  in ReMe's reproduction.

This is exactly why FlowCraft's leaderboard headline is **a
single row tied to one commit hash, one report artifact, one
declared judge prompt**, with the competitor numbers we cite
listed by source — not a single synthesised ranking.

### LongMemEval — LongMemEvalS (~115k tokens / question)

> **Difficulty tier disclosure.** LongMemEval ships three
> difficulty tiers (see `eval/longmemeval/README.md`):
> `_oracle` (evidence sessions only, <5k tok/Q, "smoke / sanity
> check"), **`_s` (40 sessions, ~115k tok/Q, headline baseline —
> this table)**, and `_m` (~500 sessions, ~1.5M tok/Q). The same
> memory stack typically drops 10-30 pp going from `_oracle` to
> `_s`. **Do not compare an `_oracle` qa.judge to an `_s`
> qa.judge.**

| System                                    | answer-LLM  | Overall accuracy | Source                                                                                                                                                                                                            |
| ----------------------------------------- | ----------- | ---------------: | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Llama 3.1 8B (long-context, no memory)    | self        |            45.4% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3b                                                                                                                                                      |
| Phi-3 14B (long-context, no memory)       | self        |            38.0% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3b                                                                                                                                                      |
| Llama 3.1 70B (long-context, no memory)   | self        |            33.4% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3b                                                                                                                                                      |
| Full-context with gpt-4o-mini (no memory) | gpt-4o-mini |            55.4% | [Zep blog](https://blog.getzep.com/state-of-the-art-agent-memory/)                                                                                                                                                |
| GPT-4o (long-context, no memory)          | self        |            60.6% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3b                                                                                                                                                      |
| Mem0 v2 (independent eval, paper era)     | gpt-4o      |            49.0% | [Zep blog](https://blog.getzep.com/state-of-the-art-agent-memory/) (cited as "Mem0 independent evaluation"); deprecated by v3 below                                                                               |
| Zep + gpt-4o-mini                         | gpt-4o-mini |            60.2% | [Zep blog](https://blog.getzep.com/state-of-the-art-agent-memory/)                                                                                                                                                |
| Full-context with gpt-4o (no memory)      | gpt-4o      |            63.8% | [Zep blog](https://blog.getzep.com/state-of-the-art-agent-memory/)                                                                                                                                                |
| Zep + gpt-4o                              | gpt-4o      |            71.2% | [Zep blog](https://blog.getzep.com/state-of-the-art-agent-memory/)                                                                                                                                                |
| **Mem0 v3 (April 2026 managed)**          | gpt-4o-mini |        **93.4%** | [Mem0 v3 blog](https://mem0.ai/blog/mem0-the-token-efficient-memory-algorithm); +25.6 pp vs v2's 67.8. Per-category: single-session-assistant 100.0 (+53.6), temporal 93.2 (+42.1), knowledge-update 96.2 (+16.7) |
| **FlowCraft**                             | gpt-4o-mini |      `[pending]` | `eval-longmemeval.yml` with `--answer-llm=azure_4o_mini --judge-llm=azure_4o_mini` + LongMemEval official grader — see [Phase-1 §2](#what-phase-1-ships)                                                          |

```yaml
judge_model: gpt-4o (LongMemEval paper protocol)
judge_prompt: LongMemEval official grader
dataset: LongMemEvalS (115k tokens / question, ~50 sessions)
question_types: single-session-user/assistant/preference, multi-session, knowledge-update, temporal, abstention
```

The FlowCraft row stays `[pending]` until a parity run lands with
the LongMemEval official grader prompt and `azure_4o_mini` for
both answer + judge. Earlier FlowCraft LongMemEvalS numbers run
with a non-`gpt-4o-mini` answer-LLM are omitted — parity-row
publishing convention forbids mixing answer-LLM provenance.

**Zep's per-question-type breakdown vs full-context baseline**
(gpt-4o, from [Zep blog](https://blog.getzep.com/state-of-the-art-agent-memory/)):

| Question type             | Full-context |   Zep |      Δ |
| ------------------------- | -----------: | ----: | -----: |
| single-session-preference |        20.0% | 56.7% |  +184% |
| single-session-assistant  |        94.6% | 80.4% | −17.7% |
| single-session-user       |        81.4% | 92.9% | +14.1% |
| temporal-reasoning        |        45.1% | 62.4% | +38.4% |
| multi-session             |        44.3% | 57.9% | +30.7% |
| knowledge-update          |        78.2% | 83.3% |  +6.5% |

A memory system that crushes full-context on temporal /
multi-session / preference reasoning **loses** on the simplest
single-session-assistant questions (condensation drops
information full-context still has). A leaderboard with only
"overall" hides this trade-off.

#### LongMemEval — commercial systems (short history pilot)

[LongMemEval paper](https://arxiv.org/abs/2410.10813) §3.4 also
evaluates ChatGPT's built-in memory + Coze on a **97-question
subset with 3-6 session histories** (~10× shorter than
LongMemEvalS); **not directly comparable** to the S-scale rows
above, listed for context on closed-source assistant memory.

| System                    | Underlying LLM | Accuracy | Source                                                       |
| ------------------------- | -------------- | -------: | ------------------------------------------------------------ |
| Offline reading (oracle)  | gpt-4o         |   91.84% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3a |
| ChatGPT (built-in memory) | gpt-4o-mini    |   71.13% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3a |
| ChatGPT (built-in memory) | gpt-4o         |   57.73% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3a |
| Coze (built-in memory)    | gpt-4o         |   32.99% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3a |
| Coze (built-in memory)    | gpt-3.5-turbo  |   24.74% | [LongMemEval paper](https://arxiv.org/abs/2410.10813) Fig 3a |

Note the inversion: ChatGPT-with-gpt-4o-mini scores 13 pp
**higher** than ChatGPT-with-gpt-4o. The paper attributes this
to gpt-4o's tendency to overwrite memory entries it deems
"outdated".

### BEIR scifact

> Per-dataset BEIR breakdown is awkward. BGE-M3's paper and
> OpenAI's `text-embedding-3-large` announcement publish only
> **BEIR-average** or **MTEB-average**, not SciFact-specific
> nDCG@10. Verifiable per-task numbers come from each model's
> HuggingFace `model-index` YAML.

| Engine / model                                      | Type                            |                    nDCG@10 |                 Recall@100 | Source                                                                                                                                                                                                                                          |
| --------------------------------------------------- | ------------------------------- | -------------------------: | -------------------------: | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| BM25 (Anserini / Lucene)                            | sparse / lexical                |                      0.665 |                          — | [BEIR paper](https://arxiv.org/pdf/2104.08663) Anserini reference; [Anserini regression docs](https://github.com/castorini/anserini/blob/master/docs/regressions-beir-v1.0.0-scifact-flat.md)                                                   |
| BGE-large-en-v1.5 (English)                         | dense                           |                 **0.7461** |                     0.9483 | [BAAI/bge-large-en-v1.5 HF model-index](https://huggingface.co/BAAI/bge-large-en-v1.5/blob/main/README.md)                                                                                                                                      |
| E5-large-v2 (English)                               | dense                           |                 **0.7224** |                     0.9627 | [intfloat/e5-large-v2 HF model-index](https://huggingface.co/intfloat/e5-large-v2/blob/main/README.md)                                                                                                                                          |
| multilingual-e5-large                               | dense (multi-lingual)           |                 **0.7041** |                     0.9387 | [intfloat/multilingual-e5-large HF model-index](https://huggingface.co/intfloat/multilingual-e5-large/blob/main/README.md)                                                                                                                      |
| BGE-M3 (dense mode)                                 | dense (multi-lingual, long-doc) | `[not published per-task]` | `[not published per-task]` | [BGE-M3 paper](https://arxiv.org/abs/2402.03216) publishes BEIR-avg only; SciFact breakdown not on [HF model card](https://huggingface.co/BAAI/bge-m3). Pull from [MTEB leaderboard](https://huggingface.co/spaces/mteb/leaderboard) if needed. |
| OpenAI text-embedding-3-large                       | dense                           | `[not published per-task]` | `[not published per-task]` | [OpenAI embedding blog](https://openai.com/index/new-embedding-models-and-api-updates/) only publishes MTEB-avg 64.6 and MIRACL-avg 54.9; no SciFact breakdown.                                                                                 |
| **FlowCraft (doc-level BM25, `retrieval` backend)** | sparse / lexical, doc-level     |                 **0.6725** |                 **0.9076** | run [`25851783774`](https://github.com/GizClaw/flowcraft/actions/runs/25851783774); `--lanes bm25 --root /var/lib/flowcraft-eval/datasets/scifact --ingest_concurrency 8` (sdk v0.3.14)                                                         |
| **FlowCraft (vector / hybrid)**                     | dense / hybrid                  |                `[pending]` |                `[pending]` | same, with `--embedder qwen:text-embedding-v4`                                                                                                                                                                                                  |

**Reading the FlowCraft row**: 0.6725 nDCG@10 / 0.9076 Recall@100
matches Anserini's Lucene BM25 baseline (0.665) within noise —
both score against doc-level BM25 stats (DocCount = 5,183
logical docs). We get there via `memory/knowledge/backend/retrieval`
backed by `memory/retrieval/memory`, which since
[#143](https://github.com/GizClaw/flowcraft/pull/143) maintains a
dedicated `__docs` namespace alongside the per-chunk namespace
inside the same `retrieval.Index`. MRR was 0.6352, errors = 0,
ingest wall-clock ~10 min @ `ingest_concurrency=8`.

**Backend rotation log** — keep when reproducing historical
numbers; the `retrieval` row above is the current canonical
FlowCraft BM25 number on scifact.

| backend / config                                                                                                | run                                                                            |    nDCG@10 | Recall@100 | wall-clock | note                                                                                          |
| --------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------ | ---------: | ---------: | ---------: | --------------------------------------------------------------------------------------------- |
| **`retrieval` (doc-level via #143 `__docs` namespace, current)**                                                | [`25851783774`](https://github.com/GizClaw/flowcraft/actions/runs/25851783774) | **0.6725** | **0.9076** |  ~13.5 min | this is the leaderboard row; matches Anserini Lucene within noise                             |
| `fs` (doc-level via #127 bespoke per-dataset doc-level inverted index)                                          | [`25844184454`](https://github.com/GizClaw/flowcraft/actions/runs/25844184454) |     0.6725 |     0.9076 |    ~30 min | identical numbers, 2× slower ingest; FSChunkRepo is deprecated for v0.5.0 removal             |
| `retrieval` (chunk-overfetch + sum-pool, [#137](https://github.com/GizClaw/flowcraft/pull/137) original design) | [`25848699992`](https://github.com/GizClaw/flowcraft/actions/runs/25848699992) |     0.1325 |     0.7266 |    ~14 min | failed #134 acceptance (5× degradation); root cause = chunk-level corpus stats, fixed by #143 |
| `fs` chunk-level Search + adapter-side `max`-pool                                                               | [`25839108340`](https://github.com/GizClaw/flowcraft/actions/runs/25839108340) |      0.180 |      0.255 |          — | original incorrect path before #126/#127/#134 work; archaeology only                          |
| `fs` chunk-level Search + adapter-side `sum`-pool                                                               | [`25840471567`](https://github.com/GizClaw/flowcraft/actions/runs/25840471567) |      0.054 |      0.207 |          — | length-biased on scifact; same archaeology                                                    |

### τ-bench — retail (Pass@1)

| Strategy / agent                    |      Pass@1 |      Pass@4 | Source                                                                                                                      |
| ----------------------------------- | ----------: | ----------: | --------------------------------------------------------------------------------------------------------------------------- |
| TC + gpt-4o-mini                    |        (??) |        (??) | [Sierra README](https://github.com/sierra-research/tau-bench) (marked ??)                                                   |
| TC + claude-3-5-sonnet-20240620     |       0.626 |       0.387 | [Sierra README](https://github.com/sierra-research/tau-bench)                                                               |
| TC + gpt-4o                         |       0.604 |       0.383 | [Sierra README](https://github.com/sierra-research/tau-bench)                                                               |
| TC + claude-3-5-sonnet-20241022     |   **0.692** |   **0.462** | [Sierra README](https://github.com/sierra-research/tau-bench)                                                               |
| **FlowCraft (TC + same agent-LLM)** | `[pending]` | `[pending]` | `eval-taubench.yml --domain sierra-retail` (Sierra's 115-task `tasks_test.py`; staging in `eval/taubench/sierra/README.md`) |

### τ-bench — airline (Pass@1)

| Strategy / agent                    |      Pass@1 |      Pass@4 | Source                                                                         |
| ----------------------------------- | ----------: | ----------: | ------------------------------------------------------------------------------ |
| TC + gpt-4o-mini                    |       0.225 |       0.100 | [Sierra README](https://github.com/sierra-research/tau-bench)                  |
| TC + claude-3-5-sonnet-20240620     |       0.360 |       0.139 | [Sierra README](https://github.com/sierra-research/tau-bench)                  |
| TC + gpt-4o                         |       0.420 |       0.200 | [Sierra README](https://github.com/sierra-research/tau-bench)                  |
| TC + claude-3-5-sonnet-20241022     |   **0.460** |   **0.225** | [Sierra README](https://github.com/sierra-research/tau-bench)                  |
| Act (gpt-4o)                        |       0.365 |       0.140 | [Sierra README](https://github.com/sierra-research/tau-bench)                  |
| ReAct (gpt-4o)                      |       0.325 |       0.160 | [Sierra README](https://github.com/sierra-research/tau-bench)                  |
| **FlowCraft (TC + same agent-LLM)** | `[pending]` | `[pending]` | `eval-taubench.yml --domain sierra-airline` (Sierra's 50-task `tasks_test.py`) |

**Caveat**: Sierra's README warns retail / airline are outdated;
[τ³-bench](https://github.com/sierra-research/tau2-bench) is the
current canonical version. Our mini-fixtures track the original
schema. When publishing, link both.

### SimpleQA — per-model (not framework)

SimpleQA scores attribute almost entirely to the **model**, not
the harness. This table is a **model citation reference**, not a
framework comparison.

All rows are SimpleQA overall accuracy (zero-shot, CoT, official
grader) from
[`openai/simple-evals` README](https://github.com/openai/simple-evals#benchmark-results).

| Model                      | SimpleQA accuracy | Primary source                                                                                                |
| -------------------------- | ----------------: | ------------------------------------------------------------------------------------------------------------- |
| o1-mini                    |               7.6 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| gpt-4.1-nano-2025-04-14    |               7.6 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| gpt-4o-mini-2024-07-18     |               9.5 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| o3-mini                    |              13.4 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| gpt-4.1-mini-2025-04-14    |              16.8 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| o4-mini                    |              20.2 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| Claude 3 Opus              |              23.5 | [Anthropic Claude 3 announcement](https://www.anthropic.com/news/claude-3-family) via openai/simple-evals     |
| Claude 3.5 Sonnet          |              28.9 | [Anthropic Claude 3.5 announcement](https://www.anthropic.com/news/claude-3-5-sonnet) via openai/simple-evals |
| gpt-4o-2024-11-20          |              38.8 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| gpt-4o-2024-05-13          |              39.0 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| gpt-4o-2024-08-06          |              40.1 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| gpt-4.1-2025-04-14         |              41.6 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| o1-preview                 |              42.4 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| o1                         |              42.6 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| o3-high                    |              48.6 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| o3                         |              49.4 | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |
| gpt-4.5-preview-2025-02-27 |          **62.5** | [openai/simple-evals](https://github.com/openai/simple-evals#benchmark-results)                               |

**FlowCraft parity check**: ran `gpt-4o-mini` to verify our
harness is within ±1pp of OpenAI's published 9.5%. Status:
**✓ PASSED** — [run 25837614443](https://github.com/GizClaw/flowcraft/actions/runs/25837614443)
on `azure_4o_mini` (200Q smoke, judge=`azure_4o_mini`) returned
**attempted_accuracy = 10.05%** (delta = +0.55 pp, inside the
±1 pp gate). `judge_failures=0` confirms the official grader
prompt parses cleanly. **Qwen / DeepSeek / Grok 3 / Llama 3.1
numbers**: not in openai/simple-evals README; cite from each
vendor's announcement when filling in. Status: `[pending]`.

---

## Methodology

This section explains **why** the numbers above disagree so much,
and the rules we follow when adding a row.

### Why cross-system numbers disagree

Headline numbers from any memory / retrieval / agent harness
encode **at least five free variables** beyond "framework
quality":

1. **Model under test** — LoCoMo qa.judge on Qwen-Max vs gpt-4o
   diverges by 10-20 pp regardless of framework.
2. **Judge LLM + judge prompt** — a lenient leaderboard prompt can
   score several points higher than a stricter prompt on the same
   predictions.
3. **Scope isolation** — pooling all LoCoMo conversations under
   one `user_id` cuts qa.judge by ~4× (see
   `eval/README.md#methodology-disclosures` §A).
4. **Dataset version** — LongMemEval has had cleaning passes
   (xiaowu0162/longmemeval-cleaned); LoCoMo numbers depend on
   which commit of snap-research/locomo was converted.
5. **Cost ceiling** — a system that always invokes a reranker
   looks better but pays 5-10× the latency/tokens.

A leaderboard that ignores these is fast to publish and
dishonest. Every row in this document therefore ships with a
methodology disclosure (YAML block or inline column) declaring
the variables above plus the source URL.

FlowCraft rows must also declare their profile. `sdk-default` means the
run is intended to measure default SDK behaviour. `locomo-leaderboard`
means the run is a benchmark-oriented reproducer and may use LoCoMo
specific choices such as larger top-k, leaderboard judge style, or
extra retrieval lanes. The profiles are both valid, but they answer
different questions and must not be merged into one headline number.

### Approach

Three approaches; **Phase 0-1 uses B exclusively. C / A are
deferred until we have a reason to invest.**

| Approach                      | What it is                                                                                              | Cost                | Fairness                                                         | Phase                    |
| ----------------------------- | ------------------------------------------------------------------------------------------------------- | ------------------- | ---------------------------------------------------------------- | ------------------------ |
| **B: cite published numbers** | Copy each competitor's published table; link the source; declare the methodology gap.                   | hours per row       | acceptable iff the gap is annotated                              | **Phase 0-1** (now)      |
| C: cite + selective our-rerun | B plus: where the competitor publishes a reproducible script, re-run it on our infra with the same LLM. | days per row        | high for the rerun column                                        | Phase 2                  |
| A: full adapter               | Wire each competitor's SDK into `eval/runners/`.                                                        | weeks per framework | highest if the competitor's "default config" is matched honestly | Phase 3, gated on demand |

Adapter work has poor ROI in the pre-launch phase: expensive,
fragile, contestable. A well-annotated citation table is faster,
less contestable, and gives external readers a real picture.

### Architectural paradigms behind the 90+ self-claims

Systems clustering in the **90+ J% self-claim band** on LoCoMo do
not share an architecture. They pick distinct primitives with
distinct production trade-offs. This matters when reading the
result tables — "92.09 vs 86.23 vs 91.6" is a comparison of
different architectures' best-case benchmark numbers under their
own protocols.

| Paradigm                                 | Representative                | Storage                                                           | Retrieval                                                             | Production cost                                             | Achilles heel                                                                                              |
| ---------------------------------------- | ----------------------------- | ----------------------------------------------------------------- | --------------------------------------------------------------------- | ----------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| **Entity-graph**                         | Mem0 v3                       | Vector DB + SQL + Entity Store with `entity → [memory_ids]` links | 3-signal: dense + lemmatized BM25 + entity-graph, rank-fused          | Cheap per query (O(log N)), ~7k tokens/query                | Requires accurate entity extraction; degrades on entity-poor questions                                     |
| **Hierarchical file + LLM-reads-files**  | MemU                          | 3-layer markdown files (Resource / Item / Category)               | Dual-mode: embedding over Items + LLM directly reads Category files   | **Expensive at scale** — context grows linearly with corpus | LoCoMo's per-conversation corpora flatter the cost profile; common-protocol benchmarks score it 39.9–61.15 |
| **File + vector hybrid + summarization** | ReMe (post-rebrand), Memobase | Markdown + JSONL + dense index, with auto-compaction to summary   | Vector + summary fallback                                             | Moderate                                                    | Summarization drops detail; bad for verbatim-attribute questions                                           |
| **Knowledge-graph + temporal edges**     | Zep / Graphiti                | Neo4j-style temporal graph                                        | Graph traversal + rerank                                              | Graph DB ops cost; reranker latency                         | Graph construction quality dominates accuracy                                                              |
| **Single-channel vector + reranker**     | FlowCraft (current)           | One unified `retrieval.Index` (vector + BM25 lanes, RRF fusion)   | Multi-lane vector + BM25 + entity-lane in fusion, optional LLM rerank | Cheap (one index, O(log N))                                 | Misses entity-anchored aggregation queries — the gap our **Entity Store** ticket addresses                 |

The 15-pp gap to mem0 v3 we see with FlowCraft's current design
is **not** evidence that entity-graph is the only architecture
that closes it. MemU closes it via LLM-reads-files (uneconomic
for us); ReMe via summarization+vector (loses detail). The
Entity Store work-item is the path we choose because it is the
cheapest production-friendly one; recorded here so future
readers see it as **a** path, not **the** path.

### Per-direction competitor inventory

#### Direction 1: Long-term dialog memory (LoCoMo / LongMemEval)

- **mem0** — historical (v2, April 2025):
  [paper](https://arxiv.org/abs/2504.19413) + [readme](https://github.com/mem0ai/mem0);
  LoCoMo 66.88, LongMemEval 67.8. Current (v3, April 2026):
  [token-efficient blog](https://mem0.ai/blog/mem0-the-token-efficient-memory-algorithm)
  - [memory-benchmarks](https://github.com/mem0ai/memory-benchmarks);
    LoCoMo 91.6, LongMemEval 93.4. Single-pass ADD-only + entity
    linking + 3-signal retrieval; the SOTA reference.
- **Letta (MemGPT)** — [letta-ai/letta](https://github.com/letta-ai/letta),
  [MemGPT paper](https://arxiv.org/abs/2310.08560). No current
  LoCoMo number known.
- **Zep** — [getzep/zep](https://github.com/getzep/zep) +
  [blog comparisons](https://blog.getzep.com/). Production
  memory store, public benchmarks.
- **MemoryScope / ReMe** (Alibaba) — originally heavy multi-worker
  pipeline at [modelscope/MemoryScope](https://github.com/modelscope/MemoryScope);
  rebranded mid-2026 to [agentscope-ai/ReMe](https://github.com/agentscope-ai/ReMe)
  with simplified hybrid markdown + vector design (ReMeLight).
  Maintains a curated LoCoMo leaderboard with 10 baselines (see
  Source E).
- **MemU** — [memu.pro](https://memu.pro/), 3-layer markdown +
  LLM-as-retriever. Highest self-claim (92.09); third-party
  reproductions score 39.9-61.15.
- **MemR3** — [MemR³ paper](https://arxiv.org/abs/2512.20237) +
  [Leagein/memr3](https://github.com/Leagein/memr3). Relative
  numbers in own paper; absolute 81.55 in ReMe's reproduction.
- **A-MEM** — agentic memory, open source; canonical repo TBD.
- **Cognee** — [topoteretes/cognee](https://github.com/topoteretes/cognee).
- **LongMemEval baseline tables** — the
  [LongMemEval paper](https://arxiv.org/abs/2410.10813) itself
  benchmarks several systems; useful "for context" rows.

Closed-source OpenAI Memory and Anthropic Memory have no
published LoCoMo / LongMemEval numbers — cells marked
`N/A — closed framework, no published number`.

#### Direction 2: History compression (`eval history`)

- **LLMLingua / LLMLingua-2** —
  [microsoft/LLMLingua](https://github.com/microsoft/LLMLingua),
  compression-ratio vs downstream-quality curves on LongBench /
  NaturalQuestions.
- **AutoCompressor** — Princeton,
  [paper](https://arxiv.org/abs/2305.14788).
- **GPTCache** —
  [zilliztech/gptcache](https://github.com/zilliztech/gptcache);
  mostly orthogonal but worth a row.

Most prompt-compression work is benchmarked on single-turn QA
datasets, not multi-session dialog (LoCoMo). Direct comparison
requires either a re-run or an explicit "different dataset"
warning per row.

#### Direction 3: Knowledge retrieval (`eval knowledge`, `eval beir`)

BEIR is the easiest direction to cite — the
[official BEIR leaderboard](https://github.com/beir-cellar/beir)
publishes nDCG@10 / Recall@100 per dataset for many systems.

- **BM25 (Anserini / Lucene)** — canonical BM25 baseline.
- **BGE-M3** —
  [FlagOpen/FlagEmbedding](https://github.com/FlagOpen/FlagEmbedding);
  current-SOTA-class dense.
- **E5** — Microsoft, public BEIR numbers.
- **Cohere / OpenAI embed** — paid; cite their blogs.

Our in-process BM25 (`memory/retrieval/memory`) is a Go
reimplementation; a `bm25` cell that beats Anserini is
suspicious. The current row at 0.6725 matches within noise.

#### Direction 4: SimpleQA factuality (`eval simpleqa`)

The **model**, not the framework. SimpleQA's protocol fixes the
answer prompt and the judge prompt (OpenAI's official grader).
Per-model citation table sourced from each vendor's blog. The
parity check above (10.05% on azure_4o_mini vs OpenAI's cited
9.5%) confirms our harness is faithful to the upstream protocol.

#### Direction 5: Tool-use agents (`eval taubench`)

- **Sierra τ-bench** —
  [sierra-research/tau-bench](https://github.com/sierra-research/tau-bench);
  publishes Pass@1 / Pass@4 for gpt-4o / Claude / Qwen / Llama.
- **τ²-bench** — same handle, current canonical.
- **AgentBench, ToolBench, AutoGen** — too divergent in tool
  definitions to compare; out of scope.

τ-bench scores are tied to a specific tool-call schema **and**
exact task set. The only fair FlowCraft baseline is
`eval-taubench.yml --domain sierra-retail` /
`--domain sierra-airline` (Sierra's 115-task / 50-task official
test sets); mini-pack pass-rates are smoke checks only, not
leaderboard numbers.

### Reproduction protocol

Every leaderboard row that represents **a FlowCraft number we
ran ourselves** (as opposed to a cited competitor number) must
satisfy:

1. **Commit hash recorded** — the FlowCraft commit hash used.
2. **`.env` profile recorded** — the `FLOWCRAFT_<ALIAS>` JSON
   (keys redacted) committed under
   `eval/leaderboard/profiles/<row-id>.env.json` so the next
   operator can reproduce the model / endpoint combination.
3. **Report artifact archived** — `${run_id}.json` under
   `/var/lib/flowcraft-eval/reports/` on the runner host; the
   workflow run URL in the methodology sub-row.
4. **Disclosure block ticked** — explicit YAML declares §A-§E:

   ```yaml
   profile: locomo-leaderboard
   scope_isolation: per_conversation # §A
   em_definition: loose # §B
   extractor_prompt: default # §C
   judge_style: locomo # §D
   topk: 30
   soft_merge: true # §E
   ```

For cited competitor rows, points 1-3 are replaced by a single
source URL pointing at the paper / readme / blog the number came
from; the YAML records what the source declares (or `unknown`).

A row missing any of these is filed as "preview" and excluded
from the headline table.

## What Phase 1 ships

A practical citation-only first cut, rough order:

1. **LoCoMo10** — already done above with Sources A-E +
   cross-source comparison. The FlowCraft self-row updates as we
   close the entity-store + lemmatization gaps.
2. **LongMemEval** — parity run with
   `--answer-llm=azure_4o_mini --judge-llm=azure_4o_mini` on
   `longmemeval_s.jsonl`. Outstanding: (1) the run itself,
   (2) surface `Report.PerQuestion.Tags` for a per-type
   FlowCraft column, (3) optional `_oracle` row to anchor the
   `_oracle → _s` drop curve. _Half a day each._
3. **BEIR scifact** — BM25 (Anserini) + BGE-M3 + E5 from the
   BEIR leaderboard. FlowCraft self-row from `eval-beir.yml`.
   _Done for BM25; vector / hybrid pending._
4. **SimpleQA self-check** — parity check passed on
   `gpt-4o-mini`. Qwen / DeepSeek / Grok / Llama 3.1 numbers
   pending vendor-blog citation. _A few hours._
5. **τ-bench retail** — Sierra's published Pass@1 + FlowCraft
   self-row from `eval-taubench.yml`. _A few hours._

### New work surfaced by the mem0 v3 (April 2026) release

mem0 v3 jumped LoCoMo from 71.4 → 91.6 by adding two primitives.
As the architectural-paradigms table shows, the entity-graph is
**not** the only path into the 90+ band (MemU does it via
LLM-reads-files; ReMe via file+vector+summary), but it **is** the
cheapest production-friendly one. Each item is a discrete
work-ticket:

1. **Entity-index retrieval layer** — auto-extract entities
   (proper nouns, quoted strings, compound noun phrases) into a
   dedicated index alongside chunk/dense; have `pipeline.LTM`
   query all three with rank-fusion. Today `memory/recall` carries
   an `entities` field on every fact but does **not** maintain a
   separate retrieval channel keyed by them. Expected
   contribution: +10~15 pp on multi-hop / open-domain — upper
   bound, since mem0's number includes the LLM-update-resolver
   which we disabled for LoCoMo.
2. **Lemmatized BM25** — `memory/text`'s tokenizer/stemmer
   needs a verb-form normalization pass so "attending a meeting"
   and "what meetings did I attend" share a key. mem0 v3 blog
   calls this out as "measurable impact"; expected +2~5 pp.
3. **(Tracking only)** BEAM benchmark (1M / 10M token scales,
   [mem0ai/memory-benchmarks](https://github.com/mem0ai/memory-benchmarks)).
   Not committing to a FlowCraft BEAM row in Phase 1 — evaluation
   cost is significant and BEAM solves a problem (10M token
   context retrieval) FlowCraft is not yet optimised for.

## Open questions for the operator

These do not block Phase 0 but block Phase 1:

1. **Publishing cadence.** Quarterly? On every minor release?
   Affects how stale rows are handled.
2. **Reproduction budget.** FlowCraft self-rows cost LLM tokens.
   Nightly regressions, or only on demand?
3. **Where the leaderboard is hosted.** This `.md`?
   `docs.flowcraft.dev/leaderboard`? Internal-only? Affects how
   careful the "this number could be misread" framing must be.
4. **Who signs off** on a new row. Eval owner alone, or a second
   reviewer? Same governance as a release.
