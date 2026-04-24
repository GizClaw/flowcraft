package e2e_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/knowledge/factory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const (
	corpusDir   = "testdata/corpus"
	goldenPath  = "testdata/golden.jsonl"
	datasetID   = "e2e"
	defaultTopK = 5
)

// goldenItem mirrors a single line of golden.jsonl. expected_doc is
// the empty string for "negative" rows that should not match anything.
type goldenItem struct {
	ID               string   `json:"id"`
	Category         string   `json:"category"`
	Question         string   `json:"question"`
	ExpectedDoc      string   `json:"expected_doc"`
	ExpectedKeywords []string `json:"expected_keywords"`
}

// thresholds bundles the per-mode pass/fail bars. They are intentionally
// generous so flake risk is low; the relative invariants (hybrid ≥
// bm25) are what catch real regressions.
type thresholds struct {
	recall   float64 // fraction of positive-class queries whose expected_doc must appear in top-K
	keyword  float64 // among the hits where expected_doc was found, fraction whose content covers all expected_keywords
	negative float64 // for negative queries, max acceptable top-1 score (0 disables this check)
}

// loadGolden parses the JSONL golden file. Lines are kept in disk order
// so test failures can be matched to the file by line number.
func loadGolden(t *testing.T) []goldenItem {
	t.Helper()
	f, err := os.Open(goldenPath)
	if err != nil {
		t.Fatalf("open golden: %v", err)
	}
	defer f.Close()
	var out []goldenItem
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var g goldenItem
		if err := json.Unmarshal([]byte(line), &g); err != nil {
			t.Fatalf("parse golden line %q: %v", line, err)
		}
		out = append(out, g)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan golden: %v", err)
	}
	return out
}

// buildService spins up a fresh in-memory workspace, ingests every
// markdown under testdata/corpus and returns a ready-to-search Service.
//
// Each test gets its own service so subtests stay isolated; the corpus
// is small enough (100 short markdowns) that the ingest cost is
// negligible. When embedder is non-nil the vector lane is also wired.
func buildService(t *testing.T, embedder knowledge.Embedder) *knowledge.Service {
	t.Helper()
	ws := workspace.NewMemWorkspace()
	opts := []factory.LocalOption{}
	if embedder != nil {
		opts = append(opts, factory.WithLocalEmbedder(embedder, "e2e"))
	}
	svc := factory.NewLocal(ws, opts...)

	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	ctx := context.Background()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(corpusDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := svc.PutDocument(ctx, datasetID, e.Name(), string(body)); err != nil {
			t.Fatalf("put %s: %v", e.Name(), err)
		}
	}
	return svc
}

// runEval scores the service against the golden set under the given
// mode and asserts the recall / keyword / negative bars in th. The
// returned recallRate is what cross-mode invariants compare on.
func runEval(t *testing.T, svc *knowledge.Service, mode knowledge.Mode, th thresholds) (recallRate float64) {
	t.Helper()
	golden := loadGolden(t)
	ctx := context.Background()

	var (
		positives    int
		recallHits   int
		keywordTotal int
		keywordOk    int
		negFails     []string
	)

	for _, g := range golden {
		res, err := svc.Search(ctx, knowledge.Query{
			DatasetID: datasetID,
			Scope:     knowledge.ScopeSingleDataset,
			Text:      g.Question,
			Mode:      mode,
			TopK:      defaultTopK,
		})
		if err != nil {
			t.Fatalf("[%s] search %q: %v", g.ID, g.Question, err)
		}

		if g.Category == "negative" {
			if th.negative > 0 && len(res.Hits) > 0 && res.Hits[0].Score > th.negative {
				negFails = append(negFails, g.ID)
			}
			continue
		}

		positives++
		matched := false
		for rank, h := range res.Hits {
			if h.DocName == g.ExpectedDoc {
				matched = true
				keywordTotal++
				if hasAllKeywords(h.Content, g.ExpectedKeywords) {
					keywordOk++
				} else {
					t.Logf("[%s] mode=%s: doc hit at rank=%d but missing keywords %v in %.80q",
						g.ID, mode, rank+1, g.ExpectedKeywords, h.Content)
				}
				break
			}
		}
		if matched {
			recallHits++
		} else {
			topNames := make([]string, 0, len(res.Hits))
			for _, h := range res.Hits {
				topNames = append(topNames, h.DocName)
			}
			t.Logf("[%s] mode=%s MISS expected=%s topK=%v question=%q",
				g.ID, mode, g.ExpectedDoc, topNames, g.Question)
		}
	}

	recallRate = ratio(recallHits, positives)
	keywordRate := ratio(keywordOk, keywordTotal)
	t.Logf("mode=%s recall@%d=%.2f (%d/%d) keyword=%.2f (%d/%d) negFails=%v",
		mode, defaultTopK, recallRate, recallHits, positives,
		keywordRate, keywordOk, keywordTotal, negFails)

	if recallRate < th.recall {
		t.Errorf("mode=%s recall@%d=%.2f below threshold %.2f", mode, defaultTopK, recallRate, th.recall)
	}
	if keywordRate < th.keyword {
		t.Errorf("mode=%s keyword=%.2f below threshold %.2f", mode, keywordRate, th.keyword)
	}
	if len(negFails) > 0 {
		t.Errorf("mode=%s negative-class queries broke score ceiling: %v", mode, negFails)
	}
	return recallRate
}

func hasAllKeywords(content string, kws []string) bool {
	for _, kw := range kws {
		if !strings.Contains(content, kw) {
			return false
		}
	}
	return true
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 1
	}
	return float64(n) / float64(d)
}
