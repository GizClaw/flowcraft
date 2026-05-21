package locomo

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

type recallDumpRecord struct {
	QID   string          `json:"qid"`
	Query string          `json:"query"`
	Gold  []string        `json:"gold_answers,omitempty"`
	Hits  []recallDumpHit `json:"hits"`
}

type recallDumpHit struct {
	ID       string   `json:"id"`
	Rank     int      `json:"rank"`
	Score    float64  `json:"score"`
	Kind     string   `json:"kind,omitempty"`
	Sources  []string `json:"sources,omitempty"`
	Content  string   `json:"content"`
	Evidence []string `json:"evidence_ids,omitempty"`
	ValidAt  string   `json:"valid_from,omitempty"`
}

type recallAnalysisRecord struct {
	QID          string             `json:"qid"`
	Query        string             `json:"query"`
	Tags         []string           `json:"tags,omitempty"`
	Judge        float64            `json:"judge"`
	Flip         string             `json:"flip,omitempty"`
	MissType     string             `json:"miss_type"`
	Prediction   string             `json:"prediction,omitempty"`
	GoldAnswers  []string           `json:"gold_answers,omitempty"`
	GoldTerms    []string           `json:"gold_terms,omitempty"`
	MissingTerms []string           `json:"missing_gold_terms,omitempty"`
	TermCoverage float64            `json:"gold_term_coverage,omitempty"`
	BestGoldRank int                `json:"best_gold_rank,omitempty"`
	TermHits     []recallTermHit    `json:"term_hits,omitempty"`
	TopKinds     map[string]int     `json:"top_kinds,omitempty"`
	TopSources   map[string]int     `json:"top_sources,omitempty"`
	TopHits      []recallTopHitView `json:"top_hits,omitempty"`
}

type recallTermHit struct {
	Term    string   `json:"term"`
	Rank    int      `json:"rank"`
	Kind    string   `json:"kind,omitempty"`
	Sources []string `json:"sources,omitempty"`
	HitID   string   `json:"hit_id,omitempty"`
}

type recallTopHitView struct {
	Rank     int      `json:"rank"`
	ID       string   `json:"id"`
	Kind     string   `json:"kind,omitempty"`
	Sources  []string `json:"sources,omitempty"`
	Evidence []string `json:"evidence_ids,omitempty"`
	Snippet  string   `json:"snippet"`
}

func addLocomoAnalyzeRecall(parent *cobra.Command) {
	var (
		baselinePath string
		category     string
		onlyErrors   bool
		onlyFlips    bool
		format       string
		outPath      string
		topN         int
	)
	cmd := &cobra.Command{
		Use:   "analyze-recall <report.json> <recall.jsonl>",
		Short: "Classify recall dump misses by gold-term rank, kind, and source",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			report, err := loadReport(args[0])
			if err != nil {
				return err
			}
			var baseline *Report
			if baselinePath != "" {
				baseline, err = loadReport(baselinePath)
				if err != nil {
					return err
				}
			}
			dumps, err := loadRecallDump(args[1])
			if err != nil {
				return err
			}
			records := analyzeRecallDump(report, baseline, dumps, analyzeRecallOptions{
				Category:   category,
				OnlyErrors: onlyErrors,
				OnlyFlips:  onlyFlips,
				TopN:       topN,
			})
			var w io.Writer = os.Stdout
			if outPath != "" {
				f, err := os.Create(outPath)
				if err != nil {
					return err
				}
				defer f.Close()
				w = f
			}
			switch format {
			case "jsonl":
				enc := json.NewEncoder(w)
				for _, rec := range records {
					if err := enc.Encode(rec); err != nil {
						return err
					}
				}
			case "markdown":
				writeRecallAnalysisMarkdown(w, records)
			default:
				return fmt.Errorf("--format must be jsonl or markdown, got %q", format)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&baselinePath, "baseline", "", "optional baseline report JSON; marks regressed/improved flips by qid")
	cmd.Flags().StringVar(&category, "category", "", "optional category tag filter, e.g. multi-hop")
	cmd.Flags().BoolVar(&onlyErrors, "only-errors", false, "only emit questions whose current judge score is 0")
	cmd.Flags().BoolVar(&onlyFlips, "only-flips", false, "only emit questions that changed judge outcome vs --baseline")
	cmd.Flags().StringVar(&format, "format", "jsonl", "output format: jsonl or markdown")
	cmd.Flags().StringVar(&outPath, "out", "", "optional output path; defaults to stdout")
	cmd.Flags().IntVar(&topN, "top", 5, "number of top hits to include per record")
	parent.AddCommand(cmd)
}

type analyzeRecallOptions struct {
	Category   string
	OnlyErrors bool
	OnlyFlips  bool
	TopN       int
}

func loadRecallDump(path string) (map[string]recallDumpRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]recallDumpRecord{}
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	sc.Buffer(buf, 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec recallDumpRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, err
		}
		out[rec.QID] = rec
	}
	return out, sc.Err()
}

func analyzeRecallDump(report, baseline *Report, dumps map[string]recallDumpRecord, opts analyzeRecallOptions) []recallAnalysisRecord {
	baseByID := map[string]QuestionScore{}
	if baseline != nil {
		for _, q := range baseline.PerQuestion {
			baseByID[q.ID] = q
		}
	}
	var out []recallAnalysisRecord
	for _, q := range report.PerQuestion {
		if opts.Category != "" && !hasTag(q.Tags, opts.Category) {
			continue
		}
		if opts.OnlyErrors && q.Judge >= 0.5 {
			continue
		}
		flip := recallFlip(q, baseByID)
		if opts.OnlyFlips && flip == "" {
			continue
		}
		dump := dumps[q.ID]
		rec := classifyRecallQuestion(q, flip, dump, opts.TopN)
		out = append(out, rec)
	}
	return out
}

func classifyRecallQuestion(q QuestionScore, flip string, dump recallDumpRecord, topN int) recallAnalysisRecord {
	gold := append([]string(nil), dump.Gold...)
	if len(gold) == 0 {
		gold = nil
	}
	terms := goldTerms(gold)
	termHits := findTermHits(terms, dump.Hits)
	missing := missingGoldTerms(terms, termHits)
	coverage := 0.0
	if len(terms) > 0 {
		coverage = float64(len(terms)-len(missing)) / float64(len(terms))
	}
	bestRank := 0
	for _, hit := range termHits {
		if bestRank == 0 || hit.Rank < bestRank {
			bestRank = hit.Rank
		}
	}
	rec := recallAnalysisRecord{
		QID:          q.ID,
		Query:        q.Query,
		Tags:         append([]string(nil), q.Tags...),
		Judge:        q.Judge,
		Flip:         flip,
		MissType:     recallMissType(q, terms, coverage, bestRank, len(dump.Hits)),
		Prediction:   q.Prediction,
		GoldAnswers:  gold,
		GoldTerms:    terms,
		MissingTerms: missing,
		TermCoverage: coverage,
		BestGoldRank: bestRank,
		TermHits:     termHits,
		TopKinds:     countTopKinds(dump.Hits),
		TopSources:   countTopSources(dump.Hits),
		TopHits:      topHitViews(dump.Hits, topN),
	}
	return rec
}

func recallMissType(q QuestionScore, terms []string, coverage float64, bestRank, hitCount int) string {
	if q.Judge >= 0.5 {
		return "correct"
	}
	if hitCount == 0 {
		return "recall_empty"
	}
	if len(terms) == 0 {
		if looksIDK(q.Prediction) {
			return "answer_abstain_unclassified"
		}
		return "answer_miss_unclassified"
	}
	if bestRank == 0 {
		return "recall_miss_gold_terms_absent"
	}
	if coverage < 0.5 {
		return "recall_miss_gold_terms_partial"
	}
	if looksIDK(q.Prediction) {
		return "answer_abstain_gold_terms_present"
	}
	if bestRank > 10 {
		return "rank_miss_gold_terms_low"
	}
	return "answer_miss_gold_terms_present"
}

func recallFlip(q QuestionScore, baseByID map[string]QuestionScore) string {
	if len(baseByID) == 0 {
		return ""
	}
	base, ok := baseByID[q.ID]
	if !ok {
		return ""
	}
	baseOK := base.Judge >= 0.5
	curOK := q.Judge >= 0.5
	switch {
	case baseOK && !curOK:
		return "regressed"
	case !baseOK && curOK:
		return "improved"
	default:
		return ""
	}
}

func findTermHits(terms []string, hits []recallDumpHit) []recallTermHit {
	var out []recallTermHit
	for _, term := range terms {
		needle := strings.ToLower(term)
		for i, hit := range hits {
			rank := hit.Rank
			if rank == 0 {
				rank = i + 1
			}
			if strings.Contains(strings.ToLower(hit.Content), needle) {
				out = append(out, recallTermHit{
					Term:    term,
					Rank:    rank,
					Kind:    hit.Kind,
					Sources: append([]string(nil), hit.Sources...),
					HitID:   hit.ID,
				})
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Rank != out[j].Rank {
			return out[i].Rank < out[j].Rank
		}
		return out[i].Term < out[j].Term
	})
	return out
}

func missingGoldTerms(terms []string, hits []recallTermHit) []string {
	if len(terms) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(hits))
	for _, hit := range hits {
		seen[hit.Term] = true
	}
	var out []string
	for _, term := range terms {
		if !seen[term] {
			out = append(out, term)
		}
	}
	return out
}

func countTopKinds(hits []recallDumpHit) map[string]int {
	out := map[string]int{}
	for _, hit := range hits {
		if hit.Kind != "" {
			out[hit.Kind]++
		}
	}
	return out
}

func countTopSources(hits []recallDumpHit) map[string]int {
	out := map[string]int{}
	for _, hit := range hits {
		for _, src := range hit.Sources {
			if src != "" {
				out[src]++
			}
		}
	}
	return out
}

func topHitViews(hits []recallDumpHit, n int) []recallTopHitView {
	if n <= 0 || len(hits) == 0 {
		return nil
	}
	if n > len(hits) {
		n = len(hits)
	}
	out := make([]recallTopHitView, 0, n)
	for i := 0; i < n; i++ {
		hit := hits[i]
		rank := hit.Rank
		if rank == 0 {
			rank = i + 1
		}
		out = append(out, recallTopHitView{
			Rank:     rank,
			ID:       hit.ID,
			Kind:     hit.Kind,
			Sources:  append([]string(nil), hit.Sources...),
			Evidence: append([]string(nil), hit.Evidence...),
			Snippet:  compactSnippet(hit.Content, 180),
		})
	}
	return out
}

func writeRecallAnalysisMarkdown(w io.Writer, records []recallAnalysisRecord) {
	fmt.Fprintln(w, "| qid | judge | flip | miss_type | gold_coverage | best_gold_rank | missing_terms | top hit |")
	fmt.Fprintln(w, "|---|---:|---|---|---:|---:|---|---|")
	for _, rec := range records {
		top := ""
		if len(rec.TopHits) > 0 {
			h := rec.TopHits[0]
			top = fmt.Sprintf("#%d %s %s", h.Rank, h.Kind, strings.Join(h.Sources, "+"))
		}
		fmt.Fprintf(w, "| %s | %.0f | %s | %s | %.2f | %d | %s | %s |\n",
			rec.QID, rec.Judge, rec.Flip, rec.MissType, rec.TermCoverage, rec.BestGoldRank, strings.Join(rec.MissingTerms, ","), top)
	}
}

var analysisWordRE = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9'_-]*`)

func goldTerms(answers []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, answer := range answers {
		for _, raw := range analysisWordRE.FindAllString(answer, -1) {
			term := strings.ToLower(strings.Trim(raw, "'_-"))
			if len(term) < 3 || analysisStopwords[term] {
				continue
			}
			if _, ok := seen[term]; ok {
				continue
			}
			seen[term] = struct{}{}
			out = append(out, term)
		}
	}
	return out
}

var analysisStopwords = map[string]bool{
	"and": true, "are": true, "but": true, "for": true, "from": true,
	"has": true, "have": true, "her": true, "him": true, "his": true,
	"likely": true, "not": true, "she": true, "that": true, "the": true,
	"their": true, "was": true, "were": true, "with": true, "yes": true,
	"you": true,
}

func looksIDK(pred string) bool {
	return strings.Contains(strings.ToLower(pred), "i don't know")
}

func compactSnippet(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
