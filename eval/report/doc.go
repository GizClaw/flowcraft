// Package report will host the unified Report schema and Compare/Render helpers
// shared by every eval suite (LoCoMo, history, knowledge, …).
//
// v0.4 introduces the cross-suite Report contract — until then each suite owns
// its own report type and the existing per-suite cmd/compare CLIs (e.g.
// eval/locomo/cmd/compare) carry the comparison logic. This package is the
// designated landing zone for that work; the migration to eval/ deliberately
// kept suite semantics untouched, so this file is a placeholder anchor for
// the package path and nothing else.
package report
