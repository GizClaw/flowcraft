package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

func main() {
	log.SetFlags(0)
	in := flag.String("in", "contracts/events.yaml", "path to events manifest")
	mode := flag.String("mode", "gen", "gen | check | diff | fmt | write-baseline")
	baseline := flag.String("baseline", "cmd/eventgen/testdata/baseline/events.snapshot.yaml", "baseline snapshot for check/diff/write-baseline")
	repo := flag.String("repo", ".", "repository root")
	flag.Parse()

	root, err := filepath.Abs(*repo)
	if err != nil {
		log.Fatal(err)
	}
	manifest := filepath.Join(root, *in)
	if _, err := os.Stat(manifest); err != nil {
		log.Fatalf("manifest %s: %v", manifest, err)
	}

	spec, err := loadSpec(manifest)
	if err != nil {
		log.Fatalf("load: %v", err)
	}
	if errs := lintSpec(spec); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "events-check FAIL:", e)
		}
		os.Exit(1)
	}

	switch *mode {
	case "gen":
		outGo := filepath.Join(root, "internal/eventlog")
		outWeb := filepath.Join(root, "web/src/api")
		outContracts := filepath.Join(root, "contracts")
		if err := writeGoOutputs(spec, outGo); err != nil {
			log.Fatal(err)
		}
		if err := writeTSOutputs(spec, outWeb); err != nil {
			log.Fatal(err)
		}
		if err := writeJSONSchemas(spec, outContracts); err != nil {
			log.Fatal(err)
		}
	case "check":
		bp := filepath.Join(root, *baseline)
		if _, err := os.Stat(bp); err != nil {
			log.Fatalf("baseline %s: %v (run: go run ./cmd/eventgen -repo=%q -mode=write-baseline)", bp, err, root)
		}
		if evo := checkEvolution(spec, bp); len(evo) > 0 {
			for _, e := range evo {
				fmt.Fprintln(os.Stderr, "events-check FAIL:", e)
			}
			os.Exit(1)
		}
	case "diff":
		bp := filepath.Join(root, *baseline)
		base, err := loadSnapshot(bp)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print(diffSnapshots(buildSnapshot(spec), base))
	case "fmt":
		if err := formatContracts(filepath.Join(root, "contracts")); err != nil {
			log.Fatal(err)
		}
	case "write-baseline":
		bp := filepath.Join(root, *baseline)
		if err := os.MkdirAll(filepath.Dir(bp), 0o755); err != nil {
			log.Fatal(err)
		}
		if err := writeSnapshotFile(bp, spec); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", bp)
	default:
		log.Fatalf("unknown -mode %q", *mode)
	}
}
