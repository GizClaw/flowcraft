// Command compare prints a markdown diff between two locomo report JSONs.
//
// Usage:
//
//	go run ./bench/locomo/cmd/compare baseline.json current.json
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/GizClaw/flowcraft/bench/locomo"
	"github.com/GizClaw/flowcraft/bench/locomo/metrics"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: compare <baseline.json> <current.json>")
		os.Exit(2)
	}
	base, err := load(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	cur, err := load(os.Args[2])
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("# locomo compare — %s vs %s\n\n", base.Runner, cur.Runner)
	fmt.Printf("- baseline: %s (%d questions)\n", base.Dataset, base.N)
	fmt.Printf("- current : %s (%d questions)\n\n", cur.Dataset, cur.N)
	fmt.Println("| metric | baseline | current | delta |")
	fmt.Println("|---|---:|---:|---:|")
	row("qa.em", base.Aggregate.EM, cur.Aggregate.EM, "%.3f")
	row("qa.f1", base.Aggregate.F1, cur.Aggregate.F1, "%.3f")
	row("qa.judge", base.Aggregate.Judge, cur.Aggregate.Judge, "%.3f")
	rowOpt("recall.k_hit", base.Aggregate.KHitRate, cur.Aggregate.KHitRate, "%.3f")
	rowDur("latency.save.p95", base.Latency["save"], cur.Latency["save"])
	rowDur("latency.recall.p95", base.Latency["recall"], cur.Latency["recall"])
}

func load(path string) (*locomo.Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := &locomo.Report{}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, err
	}
	return r, nil
}

func row(name string, a, b float64, fmtStr string) {
	delta := b - a
	fmt.Printf("| %s | "+fmtStr+" | "+fmtStr+" | %+.3f |\n", name, a, b, delta)
}

// rowOpt renders an optional-float metric: either side being nil prints N/A
// and zeroes the delta, so mixed runs (raw vs extractor) don't produce
// nonsense like "delta=-0.250" when the baseline simply didn't track k_hit.
func rowOpt(name string, a, b *float64, fmtStr string) {
	if a == nil || b == nil {
		av, bv := "N/A", "N/A"
		if a != nil {
			av = fmt.Sprintf(fmtStr, *a)
		}
		if b != nil {
			bv = fmt.Sprintf(fmtStr, *b)
		}
		fmt.Printf("| %s | %s | %s | — |\n", name, av, bv)
		return
	}
	row(name, *a, *b, fmtStr)
}

func rowDur(name string, a, b metrics.LatencySummary) {
	delta := b.P95 - a.P95
	fmt.Printf("| %s | %s | %s | %s |\n", name, a.P95, b.P95, signedDur(delta))
}

func signedDur(d time.Duration) string {
	if d >= 0 {
		return "+" + d.String()
	}
	return d.String()
}
