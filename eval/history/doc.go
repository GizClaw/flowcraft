// Package history evaluates how much the sdk/history compactor trades answer
// quality for prompt-token savings on long, multi-session conversations.
//
// Where eval/locomo answers "did recall surface the right fact?", this suite
// answers "given a long transcript and no recall layer, can the model still
// answer correctly with a compressed history?". The two questions stress
// orthogonal subsystems: locomo evaluates the long-term-memory pipeline,
// history evaluates the short-term-memory compactor in isolation.
//
// The eval replays a locomo-formatted dataset three ways:
//
//   - none      — load the full transcript into the prompt (upper bound on
//     quality, lower bound on token count)
//   - buffer    — keep only the last N messages (lower bound on quality, fixed
//     prompt size)
//   - compacted — run the SummaryDAG compactor (sdk/history.NewCompacted) and
//     hand the assembled mix of summaries + recent verbatim turns to the
//     answer LLM
//
// For each strategy we report (qa.judge | qa.em | qa.f1, prompt token
// p50/p95, history.Load latency). Comparing strategies tells us whether
// the compactor's compression is "free" (matches `none`'s judge score) or
// lossy at the chosen knob settings.
//
// Run from the repo root:
//
//	# cheap CI run (synthetic dataset, EM judge, no LLMs needed for `none`/`buffer`;
//	# the `compacted` strategy is skipped when no LLM is configured).
//	go run ./eval/history/cmd/eval --dataset synthetic --out r.json
//
//	# full run (compactor enabled; requires FLOWCRAFT_QWEN JSON env or similar):
//	go run ./eval/history/cmd/eval \
//	    --dataset      eval/locomo/data/locomo10.jsonl \
//	    --answer-llm   qwen:qwen-max \
//	    --summary-llm  qwen:qwen-turbo \
//	    --judge-llm    qwen:qwen-max \
//	    --out          r.json
package history
