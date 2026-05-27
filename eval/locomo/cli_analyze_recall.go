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

	"github.com/GizClaw/flowcraft/eval/dataset"
)

type recallDumpRecord struct {
	QID             string          `json:"qid"`
	Query           string          `json:"query"`
	Gold            []string        `json:"gold_answers,omitempty"`
	Hits            []recallDumpHit `json:"hits,omitempty"` // legacy dump field
	RecallArtifacts []recallDumpHit `json:"recall_artifacts,omitempty"`
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
	QID                  string             `json:"qid"`
	Query                string             `json:"query"`
	Tags                 []string           `json:"tags,omitempty"`
	Judge                float64            `json:"judge"`
	Flip                 string             `json:"flip,omitempty"`
	MissType             string             `json:"miss_type"`
	SecondaryMiss        string             `json:"secondary_miss,omitempty"`
	Prediction           string             `json:"prediction,omitempty"`
	GoldAnswers          []string           `json:"gold_answers,omitempty"`
	EvidenceIDs          []string           `json:"evidence_ids,omitempty"`
	GoldTerms            []string           `json:"gold_terms,omitempty"`
	MissingTerms         []string           `json:"missing_gold_terms,omitempty"`
	TermCoverage         float64            `json:"gold_term_coverage,omitempty"`
	EvidenceTerms        []string           `json:"evidence_terms,omitempty"`
	MissingEvidenceTerms []string           `json:"missing_evidence_terms,omitempty"`
	EvidenceTermCoverage float64            `json:"evidence_term_coverage,omitempty"`
	BestGoldRank         int                `json:"best_gold_rank,omitempty"`
	BestEvidenceRank     int                `json:"best_evidence_rank,omitempty"`
	ExtractStatus        string             `json:"extract_status,omitempty"`
	ExtractTermCoverage  float64            `json:"extract_term_coverage,omitempty"`
	ExtractEvidenceIDHit bool               `json:"extract_evidence_id_hit,omitempty"`
	ExtractFactIDs       []string           `json:"extract_fact_ids,omitempty"`
	StageMiss            string             `json:"stage_miss,omitempty"`
	StageCoverages       map[string]float64 `json:"stage_coverages,omitempty"`
	TermHits             []recallTermHit    `json:"term_hits,omitempty"`
	TopKinds             map[string]int     `json:"top_kinds,omitempty"`
	TopSources           map[string]int     `json:"top_sources,omitempty"`
	TopHits              []recallTopHitView `json:"top_hits,omitempty"`
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

type stageAuditDumpRecord struct {
	QID    string                `json:"qid"`
	Query  string                `json:"query"`
	Gold   []string              `json:"gold_answers,omitempty"`
	Stages []stageAuditDumpStage `json:"stages,omitempty"`
}

type stageAuditDumpStage struct {
	Stage      string                    `json:"stage"`
	Source     string                    `json:"source,omitempty"`
	Status     string                    `json:"status,omitempty"`
	Candidates []stageAuditDumpCandidate `json:"candidates,omitempty"`
}

type stageAuditDumpCandidate struct {
	FactID      string   `json:"fact_id,omitempty"`
	Source      string   `json:"source,omitempty"`
	Rank        int      `json:"rank,omitempty"`
	Score       float64  `json:"score,omitempty"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	Sources     []string `json:"sources,omitempty"`
}

func addLocomoAnalyzeRecall(parent *cobra.Command) {
	var (
		baselinePath   string
		datasetPath    string
		factsPath      string
		stageAuditPath string
		category       string
		onlyErrors     bool
		onlyFlips      bool
		format         string
		outPath        string
		topN           int
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
			var signals map[string]auditSignals
			if datasetPath != "" {
				ds, err := dataset.LoadJSONL(datasetPath)
				if err != nil {
					return err
				}
				signals = buildAuditSignals(ds)
			}
			var factsByConv map[string][]factDumpFact
			var factByID map[string]factDumpFact
			if factsPath != "" {
				factRecords, err := loadFactsDump(factsPath)
				if err != nil {
					return err
				}
				factsByConv = groupFactsByConversation(factRecords)
				factByID = indexFactsByID(factRecords)
			}
			var stageAudits map[string]stageAuditDumpRecord
			if stageAuditPath != "" {
				stageAudits, err = loadStageAuditDump(stageAuditPath)
				if err != nil {
					return err
				}
			}
			records := analyzeRecallDump(report, baseline, dumps, analyzeRecallOptions{
				Category:    category,
				OnlyErrors:  onlyErrors,
				OnlyFlips:   onlyFlips,
				TopN:        topN,
				Signals:     signals,
				FactsByConv: factsByConv,
				FactByID:    factByID,
				StageAudits: stageAudits,
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
	cmd.Flags().StringVar(&datasetPath, "dataset", "", "optional dataset JSONL; enables evidence-term audit")
	cmd.Flags().StringVar(&factsPath, "facts", "", "optional --dump-facts JSONL; enables extract-vs-recall classification")
	cmd.Flags().StringVar(&stageAuditPath, "stage-audit", "", "optional --dump-stage-audit JSONL; enables source/candidate/rank attribution")
	cmd.Flags().StringVar(&category, "category", "", "optional category tag filter, e.g. multi-hop")
	cmd.Flags().BoolVar(&onlyErrors, "only-errors", false, "only emit questions whose current judge score is 0")
	cmd.Flags().BoolVar(&onlyFlips, "only-flips", false, "only emit questions that changed judge outcome vs --baseline")
	cmd.Flags().StringVar(&format, "format", "jsonl", "output format: jsonl or markdown")
	cmd.Flags().StringVar(&outPath, "out", "", "optional output path; defaults to stdout")
	cmd.Flags().IntVar(&topN, "top", 5, "number of top hits to include per record")
	parent.AddCommand(cmd)
}

type analyzeRecallOptions struct {
	Category    string
	OnlyErrors  bool
	OnlyFlips   bool
	TopN        int
	Signals     map[string]auditSignals
	FactsByConv map[string][]factDumpFact
	FactByID    map[string]factDumpFact
	StageAudits map[string]stageAuditDumpRecord
}

type auditSignals struct {
	ConversationID string
	EvidenceIDs    []string
	GoldTerms      []string
	EvidenceTerms  []string
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
		if len(rec.RecallArtifacts) > 0 {
			rec.Hits = rec.RecallArtifacts
		}
		out[rec.QID] = rec
	}
	return out, sc.Err()
}

func loadFactsDump(path string) ([]factDumpRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []factDumpRecord
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	sc.Buffer(buf, 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec factDumpRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

func groupFactsByConversation(records []factDumpRecord) map[string][]factDumpFact {
	out := map[string][]factDumpFact{}
	for _, rec := range records {
		convID := conversationIDFromScope(rec.Scope)
		if convID == "" {
			convID = rec.Scope.UserID
		}
		out[convID] = append(out[convID], rec.Facts...)
	}
	return out
}

func indexFactsByID(records []factDumpRecord) map[string]factDumpFact {
	out := map[string]factDumpFact{}
	for _, rec := range records {
		for _, fact := range rec.Facts {
			if fact.ID != "" {
				out[fact.ID] = fact
			}
		}
	}
	return out
}

func loadStageAuditDump(path string) (map[string]stageAuditDumpRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]stageAuditDumpRecord{}
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	sc.Buffer(buf, 64*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec stageAuditDumpRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, err
		}
		out[rec.QID] = rec
	}
	return out, sc.Err()
}

func buildAuditSignals(ds *dataset.Dataset) map[string]auditSignals {
	turnByConvEvidence := map[string]map[string]string{}
	for _, conv := range ds.Conversations {
		if turnByConvEvidence[conv.ID] == nil {
			turnByConvEvidence[conv.ID] = map[string]string{}
		}
		for _, turn := range conv.Turns {
			if turn.EvidenceID != "" {
				turnByConvEvidence[conv.ID][turn.EvidenceID] = turn.Content
			}
		}
	}
	out := map[string]auditSignals{}
	for _, q := range ds.Questions {
		var evidenceTexts []string
		seenEvidence := map[string]struct{}{}
		for _, id := range q.EvidenceIDs {
			if id == "" {
				continue
			}
			seenEvidence[id] = struct{}{}
			if text := turnByConvEvidence[q.ConversationID][id]; text != "" {
				evidenceTexts = append(evidenceTexts, text)
			}
		}
		evidenceIDs := make([]string, 0, len(seenEvidence))
		for id := range seenEvidence {
			evidenceIDs = append(evidenceIDs, id)
		}
		sort.Strings(evidenceIDs)
		out[q.ID] = auditSignals{
			ConversationID: q.ConversationID,
			EvidenceIDs:    evidenceIDs,
			GoldTerms:      termsFromTexts(q.GoldAnswers),
			EvidenceTerms:  termsFromTexts(evidenceTexts),
		}
	}
	return out
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
		rec := classifyRecallQuestion(q, flip, dump, opts.TopN, opts.Signals[q.ID], opts.FactsByConv, opts.FactByID, opts.StageAudits[q.ID])
		out = append(out, rec)
	}
	return out
}

func classifyRecallQuestion(q QuestionScore, flip string, dump recallDumpRecord, topN int, signals auditSignals, factsByConv map[string][]factDumpFact, factByID map[string]factDumpFact, stageAudit stageAuditDumpRecord) recallAnalysisRecord {
	gold := append([]string(nil), dump.Gold...)
	if len(gold) == 0 {
		gold = nil
	}
	terms := goldTerms(gold)
	if len(signals.GoldTerms) > 0 {
		terms = signals.GoldTerms
	}
	if signals.ConversationID == "" {
		signals.ConversationID = conversationIDFromQID(q.ID)
	}
	if len(signals.GoldTerms) == 0 {
		signals.GoldTerms = terms
	}
	evidenceTerms := append([]string(nil), signals.EvidenceTerms...)
	analysisTerms := uniqueStrings(append(append([]string{}, terms...), evidenceTerms...))
	termHits := findTermHits(terms, dump.Hits)
	missing := missingGoldTerms(terms, termHits)
	coverage := 0.0
	if len(terms) > 0 {
		coverage = float64(len(terms)-len(missing)) / float64(len(terms))
	}
	evidenceTermHits := findTermHits(evidenceTerms, dump.Hits)
	missingEvidence := missingGoldTerms(evidenceTerms, evidenceTermHits)
	evidenceCoverage := 0.0
	if len(evidenceTerms) > 0 {
		evidenceCoverage = float64(len(evidenceTerms)-len(missingEvidence)) / float64(len(evidenceTerms))
	}
	analysisHits := findTermHits(analysisTerms, dump.Hits)
	analysisMissing := missingGoldTerms(analysisTerms, analysisHits)
	analysisCoverage := 0.0
	if len(analysisTerms) > 0 {
		analysisCoverage = float64(len(analysisTerms)-len(analysisMissing)) / float64(len(analysisTerms))
	}
	bestRank := 0
	for _, hit := range termHits {
		if bestRank == 0 || hit.Rank < bestRank {
			bestRank = hit.Rank
		}
	}
	bestEvidenceRank := 0
	for _, hit := range evidenceTermHits {
		if bestEvidenceRank == 0 || hit.Rank < bestEvidenceRank {
			bestEvidenceRank = hit.Rank
		}
	}
	bestAnalysisRank := 0
	for _, hit := range analysisHits {
		if bestAnalysisRank == 0 || hit.Rank < bestAnalysisRank {
			bestAnalysisRank = hit.Rank
		}
	}
	extract := extractAuditResult{}
	if len(factsByConv) > 0 {
		extract = auditExtract(signals, factsByConv[signals.ConversationID])
	}
	stage := stageAuditResult{}
	if len(stageAudit.Stages) > 0 {
		stage = auditRecallStages(signals, analysisTerms, stageAudit, factByID)
	}
	rec := recallAnalysisRecord{
		QID:                  q.ID,
		Query:                q.Query,
		Tags:                 append([]string(nil), q.Tags...),
		Judge:                q.Judge,
		Flip:                 flip,
		MissType:             recallMissType(q, terms, coverage, bestRank, len(dump.Hits)),
		Prediction:           q.Prediction,
		GoldAnswers:          gold,
		EvidenceIDs:          append([]string(nil), signals.EvidenceIDs...),
		GoldTerms:            terms,
		MissingTerms:         missing,
		TermCoverage:         coverage,
		EvidenceTerms:        evidenceTerms,
		MissingEvidenceTerms: missingEvidence,
		EvidenceTermCoverage: evidenceCoverage,
		BestGoldRank:         bestRank,
		BestEvidenceRank:     bestEvidenceRank,
		ExtractStatus:        extract.Status,
		ExtractTermCoverage:  extract.TermCoverage,
		ExtractEvidenceIDHit: extract.EvidenceIDHit,
		ExtractFactIDs:       extract.FactIDs,
		StageMiss:            stage.Miss,
		StageCoverages:       stage.Coverages,
		TermHits:             termHits,
		TopKinds:             countTopKinds(dump.Hits),
		TopSources:           countTopSources(dump.Hits),
		TopHits:              topHitViews(dump.Hits, topN),
	}
	if len(factsByConv) > 0 {
		rec.MissType = recallAuditMissType(q, extract, analysisTerms, analysisCoverage, bestAnalysisRank, len(dump.Hits))
	}
	if len(stageAudit.Stages) > 0 {
		rec.MissType = recallStageAuditMissType(q, extract, stage)
	}
	rec.SecondaryMiss = secondaryMissCause(rec)
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

func recallAuditMissType(q QuestionScore, extract extractAuditResult, terms []string, coverage float64, bestRank, hitCount int) string {
	if q.Judge >= 0.5 {
		return "correct"
	}
	switch extract.Status {
	case "extract_miss", "extract_partial", "extract_semantic_drift":
		return extract.Status
	}
	if hitCount == 0 {
		return "recall_miss"
	}
	if len(terms) == 0 {
		if looksIDK(q.Prediction) {
			return "answer_abstain_unclassified"
		}
		return "answer_miss_unclassified"
	}
	if bestRank == 0 {
		return "recall_miss"
	}
	if coverage < 0.5 {
		return "context_partial"
	}
	if bestRank > 10 {
		return "rank_miss"
	}
	if looksIDK(q.Prediction) {
		return "answer_abstain"
	}
	return "answer_miss"
}

type stageAuditResult struct {
	Miss      string
	Coverages map[string]float64
}

func auditRecallStages(signals auditSignals, terms []string, audit stageAuditDumpRecord, factByID map[string]factDumpFact) stageAuditResult {
	coverages := map[string]float64{}
	stageCandidates := map[string][]stageAuditDumpCandidate{}
	for _, st := range audit.Stages {
		key := st.Stage
		switch st.Stage {
		case "candidate_fanout":
			key = "source"
		case "context_pack_input":
			key = "context_input"
		case "context_pack_reranked":
			key = "context_reranked"
		case "build_grounded_hits":
			key = "final"
		case "context_pack":
			key = "final"
		}
		stageCandidates[key] = append(stageCandidates[key], st.Candidates...)
	}
	order := []string{"source", "candidate_merge", "candidate_materialize", "candidate_merge_and_materialize", "policy_filter", "rank_input", "rank_output", "context_input", "context_reranked", "final"}
	for _, stage := range order {
		if candidates, ok := stageCandidates[stage]; ok {
			coverages[stage] = stageCandidateCoverage(terms, signals.EvidenceIDs, candidates, factByID)
		}
	}
	if len(signals.EvidenceIDs) > 0 {
		return stageAuditResult{
			Miss:      strictEvidenceStageMiss(signals.EvidenceIDs, stageCandidates, factByID),
			Coverages: coverages,
		}
	}
	miss := stageMissType(coverages)
	return stageAuditResult{Miss: miss, Coverages: coverages}
}

func recallStageAuditMissType(q QuestionScore, extract extractAuditResult, stage stageAuditResult) string {
	if q.Judge >= 0.5 {
		return "correct"
	}
	switch extract.Status {
	case "extract_miss", "extract_partial", "extract_semantic_drift":
		return extract.Status
	}
	if stage.Miss != "" {
		return stage.Miss
	}
	if looksIDK(q.Prediction) {
		return "answer_abstain"
	}
	return "answer_miss"
}

func secondaryMissCause(rec recallAnalysisRecord) string {
	if rec.Judge >= 0.5 {
		return ""
	}
	switch {
	case strings.HasPrefix(rec.MissType, "answer_miss"):
		return answerSecondaryMissCause("answer_miss", rec)
	case strings.HasPrefix(rec.MissType, "answer_abstain"):
		return answerSecondaryMissCause("answer_abstain", rec)
	case rec.MissType == "source_miss_evidence_id":
		return sourceSecondaryMissCause(rec)
	default:
		return ""
	}
}

func answerSecondaryMissCause(prefix string, rec recallAnalysisRecord) string {
	if temporalOrNumericQuestion(rec.Query) {
		return prefix + "_temporal_or_numeric_reasoning"
	}
	if rec.BestGoldRank > 0 && rec.BestGoldRank <= 5 && rec.TermCoverage >= 0.75 {
		return prefix + "_ignored_strong_context"
	}
	if rec.TermCoverage < 0.5 && rec.EvidenceTermCoverage >= 0.5 {
		return prefix + "_gold_surface_missing"
	}
	if rec.BestGoldRank > 10 {
		return prefix + "_gold_terms_low_rank"
	}
	if rec.BestGoldRank == 0 {
		return prefix + "_gold_terms_absent_or_paraphrased"
	}
	return prefix + "_context_distractor_or_reasoning"
}

func sourceSecondaryMissCause(rec recallAnalysisRecord) string {
	if rec.BestEvidenceRank == 0 {
		return "source_miss_no_semantic_hit"
	}
	if rec.ExtractEvidenceIDHit || rec.ExtractStatus == "extract_hit_evidence_id" {
		return "source_miss_true_source_recall"
	}
	switch rec.ExtractStatus {
	case "extract_hit_terms":
		return "source_miss_extract_evidence_id_gap"
	case "extract_partial":
		return "source_miss_partial_extract_grounding"
	default:
		return "source_miss_gold_id_but_term_hit"
	}
}

func temporalOrNumericQuestion(query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	for _, prefix := range []string{
		"when ", "when did ", "when was ", "when is ",
		"how long ", "how many ", "how much ",
		"what date ", "what year ", "what month ", "what day ",
		"which year ", "which month ", "which day ",
	} {
		if strings.HasPrefix(q, prefix) {
			return true
		}
	}
	return strings.Contains(q, " how long ") || strings.Contains(q, " how many ")
}

func stageMissType(coverages map[string]float64) string {
	source := coverages["source"]
	candidateMerge := coverages["candidate_merge"]
	candidateMaterialize := coverages["candidate_materialize"]
	rankInput := coverages["rank_input"]
	rankOutput := coverages["rank_output"]
	final := coverages["final"]
	if source == 0 {
		return "source_miss"
	}
	if candidateMerge+0.0001 < source {
		return "candidate_merge_drop"
	}
	if candidateMaterialize+0.0001 < candidateMerge {
		return "candidate_materialize_drop"
	}
	if rankInput > 0 && rankOutput+0.0001 < rankInput {
		return "rank_drop"
	}
	if final > 0 && final < 0.5 {
		return "context_partial"
	}
	return ""
}

func strictEvidenceStageMiss(evidenceIDs []string, stageCandidates map[string][]stageAuditDumpCandidate, factByID map[string]factDumpFact) string {
	if len(evidenceIDs) == 0 {
		return ""
	}
	hasStage := func(stage string) bool {
		_, ok := stageCandidates[stage]
		return ok
	}
	present := func(stage string) bool {
		for _, candidate := range stageCandidates[stage] {
			if candidateMatchesEvidenceID(candidate, evidenceIDs, factByID) {
				return true
			}
		}
		return false
	}
	if !present("source") {
		return "source_miss_evidence_id"
	}
	if !present("candidate_merge") {
		return "candidate_merge_drop_evidence_id"
	}
	if !present("candidate_materialize") {
		return "candidate_materialize_drop_evidence_id"
	}
	if !present("rank_output") {
		return "rank_drop_evidence_id"
	}
	hasContextInput := hasStage("context_input")
	hasContextReranked := hasStage("context_reranked")
	if hasContextInput && !present("context_input") {
		return "audit_inconsistent_evidence_id"
	}
	if hasContextReranked {
		if !present("context_reranked") {
			return "rerank_drop_evidence_id"
		}
		if !present("final") {
			return "final_limit_drop_evidence_id"
		}
		return ""
	}
	if !present("final") {
		if hasContextInput {
			return "final_selection_drop_evidence_id"
		}
		return "context_drop_evidence_id"
	}
	return ""
}

func candidateMatchesEvidenceID(candidate stageAuditDumpCandidate, evidenceIDs []string, factByID map[string]factDumpFact) bool {
	if len(evidenceIDs) == 0 {
		return false
	}
	want := map[string]struct{}{}
	for _, id := range evidenceIDs {
		if id != "" {
			want[id] = struct{}{}
		}
	}
	for _, id := range candidate.EvidenceIDs {
		if _, ok := want[id]; ok {
			return true
		}
	}
	if fact, ok := factByID[candidate.FactID]; ok {
		for _, id := range fact.EvidenceIDs {
			if _, ok := want[id]; ok {
				return true
			}
		}
	}
	return false
}

func stageCandidateCoverage(terms, evidenceIDs []string, candidates []stageAuditDumpCandidate, factByID map[string]factDumpFact) float64 {
	signals := uniqueStrings(append(append([]string{}, terms...), evidenceIDs...))
	if len(signals) == 0 {
		return 0
	}
	matched := map[string]bool{}
	for _, candidate := range candidates {
		text := strings.ToLower(candidate.FactID + " " + strings.Join(candidate.EvidenceIDs, " "))
		if fact, ok := factByID[candidate.FactID]; ok {
			text += " " + factSearchText(fact)
		}
		for _, signal := range signals {
			if strings.Contains(text, strings.ToLower(signal)) {
				matched[signal] = true
			}
		}
	}
	return float64(len(matched)) / float64(len(signals))
}

type extractAuditResult struct {
	Status        string
	TermCoverage  float64
	EvidenceIDHit bool
	FactIDs       []string
}

func auditExtract(signals auditSignals, facts []factDumpFact) extractAuditResult {
	terms := uniqueStrings(append(append([]string{}, signals.GoldTerms...), signals.EvidenceTerms...))
	_, missing := matchTermsInFacts(terms, facts)
	coverage := 0.0
	if len(terms) > 0 {
		coverage = float64(len(terms)-len(missing)) / float64(len(terms))
	}
	evidenceIDHit := factEvidenceIDHit(signals.EvidenceIDs, facts)
	status := "extract_miss"
	switch {
	case evidenceIDHit && (len(terms) == 0 || coverage >= 0.5):
		status = "extract_hit_evidence_id"
	case evidenceIDHit:
		status = "extract_semantic_drift"
	case coverage >= 0.5:
		status = "extract_hit_terms"
	case coverage > 0:
		status = "extract_partial"
	}
	return extractAuditResult{
		Status:        status,
		TermCoverage:  coverage,
		EvidenceIDHit: evidenceIDHit,
		FactIDs:       matchingFactIDs(terms, signals.EvidenceIDs, facts, 5),
	}
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
	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	writeCountTable(w, "miss_type", countRecallField(records, func(rec recallAnalysisRecord) string { return rec.MissType }))
	writeCountTable(w, "secondary_miss", countRecallField(records, func(rec recallAnalysisRecord) string { return rec.SecondaryMiss }))
	writeCountTable(w, "extract_status", countRecallField(records, func(rec recallAnalysisRecord) string { return rec.ExtractStatus }))
	writeCountTable(w, "stage_miss", countRecallField(records, func(rec recallAnalysisRecord) string { return rec.StageMiss }))
	writeCountTable(w, "category", countRecallCategories(records))
	writeCountTable(w, "top_kind", countNestedRecallFields(records, func(rec recallAnalysisRecord) map[string]int { return rec.TopKinds }))
	writeCountTable(w, "top_source", countNestedRecallFields(records, func(rec recallAnalysisRecord) map[string]int { return rec.TopSources }))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Questions")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| qid | judge | flip | miss_type | secondary_miss | extract_status | stage_miss | gold_coverage | evidence_coverage | best_gold_rank | best_evidence_rank | missing_terms | top hit |")
	fmt.Fprintln(w, "|---|---:|---|---|---|---|---|---:|---:|---:|---:|---|---|")
	for _, rec := range records {
		top := ""
		if len(rec.TopHits) > 0 {
			h := rec.TopHits[0]
			top = fmt.Sprintf("#%d %s %s", h.Rank, h.Kind, strings.Join(h.Sources, "+"))
		}
		missing := append([]string{}, rec.MissingTerms...)
		missing = append(missing, rec.MissingEvidenceTerms...)
		fmt.Fprintf(w, "| %s | %.0f | %s | %s | %s | %s | %s | %.2f | %.2f | %d | %d | %s | %s |\n",
			rec.QID, rec.Judge, rec.Flip, rec.MissType, rec.SecondaryMiss, rec.ExtractStatus, rec.StageMiss, rec.TermCoverage, rec.EvidenceTermCoverage, rec.BestGoldRank, rec.BestEvidenceRank, strings.Join(uniqueStrings(missing), ","), top)
	}
}

func writeCountTable(w io.Writer, name string, counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	fmt.Fprintf(w, "### By %s\n\n", name)
	fmt.Fprintf(w, "| %s | count |\n", name)
	fmt.Fprintln(w, "|---|---:|")
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "| %s | %d |\n", k, counts[k])
	}
	fmt.Fprintln(w)
}

func countRecallField(records []recallAnalysisRecord, pick func(recallAnalysisRecord) string) map[string]int {
	out := map[string]int{}
	for _, rec := range records {
		key := pick(rec)
		if key != "" {
			out[key]++
		}
	}
	return out
}

func countRecallCategories(records []recallAnalysisRecord) map[string]int {
	out := map[string]int{}
	for _, rec := range records {
		for _, tag := range rec.Tags {
			if tag != "" {
				out[tag]++
			}
		}
	}
	return out
}

func countNestedRecallFields(records []recallAnalysisRecord, pick func(recallAnalysisRecord) map[string]int) map[string]int {
	out := map[string]int{}
	for _, rec := range records {
		for key, count := range pick(rec) {
			if key != "" {
				out[key] += count
			}
		}
	}
	return out
}

var analysisWordRE = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9'_-]*`)

func goldTerms(answers []string) []string {
	return termsFromTexts(answers)
}

func termsFromTexts(texts []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, text := range texts {
		for _, raw := range analysisWordRE.FindAllString(text, -1) {
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

func uniqueStrings(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func matchTermsInFacts(terms []string, facts []factDumpFact) ([]string, []string) {
	var matched []string
	var missing []string
	for _, term := range terms {
		needle := strings.ToLower(term)
		found := false
		for _, fact := range facts {
			if strings.Contains(factSearchText(fact), needle) {
				found = true
				break
			}
		}
		if found {
			matched = append(matched, term)
		} else {
			missing = append(missing, term)
		}
	}
	return matched, missing
}

func factEvidenceIDHit(evidenceIDs []string, facts []factDumpFact) bool {
	if len(evidenceIDs) == 0 {
		return false
	}
	want := map[string]struct{}{}
	for _, id := range evidenceIDs {
		want[id] = struct{}{}
	}
	for _, fact := range facts {
		for _, id := range fact.EvidenceIDs {
			if _, ok := want[id]; ok {
				return true
			}
		}
	}
	return false
}

func matchingFactIDs(terms, evidenceIDs []string, facts []factDumpFact, limit int) []string {
	if limit <= 0 {
		return nil
	}
	wantEvidence := map[string]struct{}{}
	for _, id := range evidenceIDs {
		wantEvidence[id] = struct{}{}
	}
	var out []string
	seen := map[string]struct{}{}
	for _, fact := range facts {
		if fact.ID == "" {
			continue
		}
		matched := false
		for _, id := range fact.EvidenceIDs {
			if _, ok := wantEvidence[id]; ok {
				matched = true
				break
			}
		}
		text := factSearchText(fact)
		if !matched {
			for _, term := range terms {
				if strings.Contains(text, strings.ToLower(term)) {
					matched = true
					break
				}
			}
		}
		if !matched {
			continue
		}
		if _, ok := seen[fact.ID]; ok {
			continue
		}
		seen[fact.ID] = struct{}{}
		out = append(out, fact.ID)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func factSearchText(f factDumpFact) string {
	return strings.ToLower(strings.Join(append([]string{
		f.Content,
		f.Subject,
		f.Predicate,
		f.Object,
		f.EvidenceText,
		f.ValidFrom,
	}, f.Entities...), " "))
}

func conversationIDFromScope(scope factDumpScope) string {
	if scope.UserID == "" {
		return ""
	}
	parts := strings.Split(scope.UserID, "::")
	return parts[len(parts)-1]
}

func conversationIDFromQID(qid string) string {
	idx := strings.LastIndex(qid, "-q")
	if idx <= 0 {
		return ""
	}
	return qid[:idx]
}

var analysisStopwords = map[string]bool{
	"about": true, "after": true, "also": true, "before": true, "been": true,
	"and": true, "are": true, "but": true, "for": true, "from": true,
	"does": true, "had": true, "has": true, "have": true, "her": true,
	"herself": true, "him": true, "his": true, "likely": true, "not": true,
	"only": true, "part": true, "refer": true, "she": true, "since": true,
	"that": true, "the": true, "their": true, "then": true, "this": true,
	"though": true, "was": true, "were": true, "what": true, "when": true,
	"where": true, "which": true, "with": true, "would": true, "yes": true,
	"you": true, "your": true,
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
