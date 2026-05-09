// Package locomo is the evaluation scaffold.
//
// Layout:
//
//	eval/locomo/
//	  ├── runners/        Memory backends to evaluate (default: flowcraft).
//	  └── cmd/            CLI entry points (fetch / ingest / eval / compare).
//
// Question/Conversation types live in eval/dataset and EM/F1/Judge/Latency
// helpers live in eval/metrics — both are shared with eval/history.
//
// The scaffold deliberately ships with a tiny synthetic CN+EN dataset so that
// `go test ./eval/locomo/...` and `go run ./eval/locomo/cmd/eval` work
// out-of-the-box without network access. Real LoCoMo / LongMemEval datasets
// are downloaded on demand by `cmd/fetch`.
package locomo
