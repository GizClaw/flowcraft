// Command fetch downloads (or shows download instructions for) external
// LoCoMo / LongMemEval datasets
//
// We do NOT vendor the datasets due to license & size; this command writes
// fetch instructions to stdout in CI and only performs network I/O when run
// interactively with --download.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	doDownload := flag.Bool("download", false, "actually download datasets (off by default)")
	flag.Parse()

	if !*doDownload {
		fmt.Fprint(os.Stdout, instructions)
		return
	}
	fmt.Fprintln(os.Stderr, "fetch: --download is not yet wired; clone manually:")
	fmt.Fprint(os.Stdout, instructions)
}

const instructions = `# LoCoMo benchmark datasets
# Place under bench/locomo/data/<name>/ and reference via --dataset path.

# 1. LoCoMo (Snap Research, CC-BY)
git clone https://github.com/snap-research/locomo bench/locomo/data/locomo
# Then convert to .jsonl:
go run ./bench/locomo/cmd/convert-locomo \
    -in  bench/locomo/data/locomo/data/locomo10.json \
    -out bench/locomo/data/locomo10.jsonl

# 2. LongMemEval
git clone https://github.com/xiaowu0162/LongMemEval bench/locomo/data/longmemeval

# 3. Flowcraft synthetic — already bundled via dataset.Synthetic().
`
