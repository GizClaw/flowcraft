# eval/beir

Public-dataset retrieval evaluation suite. Drives `sdk/knowledge` against
[BEIR](https://arxiv.org/abs/2104.08663) tasks and produces the same
graded metrics every BEIR leaderboard publishes:

- **nDCG@k** — graded, normalised
- **Recall@k** — binary
- **MRR** — mean reciprocal rank of the first relevant doc

## Why BEIR (in addition to `eval/knowledge/`)

| Suite | Corpus | Metric structure | Role |
|-------|--------|-----------------|------|
| `eval/knowledge` | hand-curated 100-doc Chinese | single-doc Recall@5 + keyword cover | deterministic PR gate |
| `eval/beir` | public BEIR tasks (SciFact, NFCorpus, …) | per-query *graded* qrels → nDCG@10 | comparable to public baselines |

Same `sdk/knowledge.Service` underneath; different scoring layer.

## Quick start

BEIR distributes every task as a self-contained zip on the public TU
Darmstadt mirror. SciFact is the smallest (≈1 k docs, ≈300 queries) and
fits a few seconds of CI:

```bash
# 1. fetch once (or check it into a private artefacts bucket)
curl -L https://public.ukp.informatik.tu-darmstadt.de/thakur/BEIR/datasets/scifact.zip \
    -o /tmp/scifact.zip
unzip -q /tmp/scifact.zip -d /tmp

# 2. BM25 lane only (no credentials)
cd eval
GOWORK=off go run ./cmd/eval beir --root /tmp/scifact --lanes bm25

# 3. Full lane comparison (FLOWCRAFT_QWEN must be set)
GOWORK=off go run ./cmd/eval beir \
    --root      /tmp/scifact \
    --embedder  qwen:text-embedding-v4 \
    --lanes     bm25,vector,hybrid \
    --out       /tmp/beir-scifact.json
```

## Dataset format

`LoadDataset(root)` reads BEIR's canonical 3-file layout:

```
<root>/
  corpus.jsonl       # {"_id":"doc1","title":"...","text":"..."}
  queries.jsonl      # {"_id":"q1","text":"..."}
  qrels/test.tsv     # query-id<TAB>corpus-id<TAB>grade
```

`grade` follows BEIR convention: 0 (irrelevant) / 1 (relevant) / 2 (highly relevant).
Recall and MRR treat any grade > 0 as relevant; nDCG uses the graded
form (`2^grade − 1` gain).

## Other supported tasks

Every BEIR task ships with the same 3-file layout, so any of these
work out of the box:

| Task        | Docs  | Queries | Domain |
|-------------|-------|---------|--------|
| `scifact`   | 5 k   | 300     | scientific claim verification |
| `nfcorpus`  | 3.6 k | 323     | bio-med IR |
| `fiqa`      | 57 k  | 6.6 k   | finance QA |
| `trec-covid`| 171 k | 50      | COVID literature |
| `quora`     | 523 k | 10 k    | duplicate-question retrieval |

For corpora above ~100 k docs expect the vector lane to dominate
wall-clock; tune `--ingest-concurrency` upward only after confirming
the embedding provider's rate-limit headroom.
