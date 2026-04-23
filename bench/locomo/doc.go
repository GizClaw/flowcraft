// Package locomo is the evaluation scaffold.
//
// Layout:
//
//	bench/locomo/
//	  ├── runners/        Memory backends to evaluate (default: flowcraft).
//	  ├── metrics/        EM, F1, LLM-as-judge, latency aggregator.
//	  ├── dataset/        Question/Conversation types and synthetic loader.
//	  └── cmd/            CLI entry points (fetch / ingest / eval / compare).
//
// The scaffold deliberately ships with a tiny synthetic CN+EN dataset so that
// `go test ./bench/locomo/...` and `go run ./bench/locomo/cmd/eval` work
// out-of-the-box without network access. Real LoCoMo / LongMemEval datasets
// are downloaded on demand by `cmd/fetch`.
package locomo
