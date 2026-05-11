// Package longmemeval is a thin re-use of eval/locomo's runner on the
// LongMemEval dataset (https://arxiv.org/abs/2410.10813, ICLR 2025).
//
// LongMemEval is the natural successor of LoCoMo: 500 questions spanning
// five core long-term-memory abilities (information-extraction, multi-
// session reasoning, knowledge-update, temporal-reasoning, abstention)
// over conversations up to 500 sessions / 115k tokens long.
//
// We do NOT vendor a new evaluator/runner here. The data shape is close
// enough to LoCoMo that `cmd/convert-longmemeval` maps it onto the same
// `eval/dataset.Dataset` schema, and `eval/locomo/cmd/eval` can then run
// it end-to-end. The split exists so future LongMemEval-specific tuning
// (per-category prompts, abstention-aware scoring) has a home.
package longmemeval
