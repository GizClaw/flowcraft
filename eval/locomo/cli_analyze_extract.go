package locomo

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/dataset"
)

type extractQualityRecord struct {
	ConversationID string              `json:"conversation_id"`
	EvidenceID     string              `json:"evidence_id"`
	Role           string              `json:"role,omitempty"`
	Status         string              `json:"status"`
	Flags          []string            `json:"flags,omitempty"`
	QuestionIDs    []string            `json:"question_ids,omitempty"`
	TextSnippet    string              `json:"text_snippet,omitempty"`
	ExpectedTerms  []string            `json:"expected_terms,omitempty"`
	MissingTerms   []string            `json:"missing_terms,omitempty"`
	TermCoverage   float64             `json:"term_coverage,omitempty"`
	FactsCount     int                 `json:"facts_count"`
	FactIDs        []string            `json:"fact_ids,omitempty"`
	KindCounts     map[string]int      `json:"kind_counts,omitempty"`
	SlotCoverage   extractSlotCoverage `json:"slot_coverage"`
	CompoundFacts  int                 `json:"compound_facts,omitempty"`
	MatchedFacts   []extractFactView   `json:"matched_facts,omitempty"`
}

type extractSlotCoverage struct {
	Kind      float64 `json:"kind,omitempty"`
	Subject   float64 `json:"subject,omitempty"`
	Predicate float64 `json:"predicate,omitempty"`
	Object    float64 `json:"object,omitempty"`
	Entities  float64 `json:"entities,omitempty"`
	ValidFrom float64 `json:"valid_from,omitempty"`
}

type extractFactView struct {
	ID        string   `json:"id,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Subject   string   `json:"subject,omitempty"`
	Predicate string   `json:"predicate,omitempty"`
	Object    string   `json:"object,omitempty"`
	Entities  []string `json:"entities,omitempty"`
	ValidFrom string   `json:"valid_from,omitempty"`
	Snippet   string   `json:"snippet"`
}

func addLocomoAnalyzeExtract(parent *cobra.Command) {
	var (
		format       string
		outPath      string
		onlyQuestion bool
	)
	cmd := &cobra.Command{
		Use:   "analyze-extract <dataset.jsonl> <facts.jsonl>",
		Short: "Audit extraction quality per evidence turn",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			ds, err := dataset.LoadJSONL(args[0])
			if err != nil {
				return err
			}
			factRecords, err := loadFactsDump(args[1])
			if err != nil {
				return err
			}
			records := analyzeExtractQuality(ds, factRecords, extractQualityOptions{OnlyQuestionEvidence: onlyQuestion})
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
				writeExtractQualityMarkdown(w, records)
			default:
				return fmt.Errorf("--format must be jsonl or markdown, got %q", format)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "jsonl", "output format: jsonl or markdown")
	cmd.Flags().StringVar(&outPath, "out", "", "optional output path; defaults to stdout")
	cmd.Flags().BoolVar(&onlyQuestion, "question-evidence-only", false, "only audit evidence ids referenced by questions")
	parent.AddCommand(cmd)
}

type extractQualityOptions struct {
	OnlyQuestionEvidence bool
}

func analyzeExtractQuality(ds *dataset.Dataset, records []factDumpRecord, opts extractQualityOptions) []extractQualityRecord {
	factsByEvidence := groupFactsByEvidenceID(records)
	questionRefs := questionIDsByEvidence(ds)
	var out []extractQualityRecord
	for _, conv := range ds.Conversations {
		for _, turn := range conv.Turns {
			if turn.EvidenceID == "" {
				continue
			}
			refs := append([]string(nil), questionRefs[turn.EvidenceID]...)
			if opts.OnlyQuestionEvidence && len(refs) == 0 {
				continue
			}
			facts := factsByEvidence[turn.EvidenceID]
			rec := classifyExtractQualityTurn(conv.ID, turn, refs, facts)
			out = append(out, rec)
		}
	}
	return out
}

func groupFactsByEvidenceID(records []factDumpRecord) map[string][]factDumpFact {
	out := map[string][]factDumpFact{}
	for _, rec := range records {
		for _, fact := range rec.Facts {
			for _, id := range fact.EvidenceIDs {
				if id != "" {
					out[id] = append(out[id], fact)
				}
			}
		}
	}
	return out
}

func questionIDsByEvidence(ds *dataset.Dataset) map[string][]string {
	out := map[string][]string{}
	for _, q := range ds.Questions {
		for _, id := range q.EvidenceIDs {
			if id != "" {
				out[id] = append(out[id], q.ID)
			}
		}
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out
}

func classifyExtractQualityTurn(convID string, turn dataset.Turn, questionIDs []string, facts []factDumpFact) extractQualityRecord {
	terms := termsFromTexts([]string{turn.Content})
	matched, missing := matchTermsInFacts(terms, facts)
	coverage := 0.0
	if len(terms) > 0 {
		coverage = float64(len(matched)) / float64(len(terms))
	}
	rec := extractQualityRecord{
		ConversationID: convID,
		EvidenceID:     turn.EvidenceID,
		Role:           turn.Role,
		TextSnippet:    compactSnippet(turn.Content, 220),
		QuestionIDs:    questionIDs,
		ExpectedTerms:  terms,
		MissingTerms:   missing,
		TermCoverage:   coverage,
		FactsCount:     len(facts),
		KindCounts:     extractKindCounts(facts),
		SlotCoverage:   extractSlotCoverageForFacts(facts),
	}
	for _, fact := range facts {
		rec.FactIDs = append(rec.FactIDs, fact.ID)
		if looksCompoundFact(fact.Content) {
			rec.CompoundFacts++
		}
		rec.MatchedFacts = append(rec.MatchedFacts, extractFactView{
			ID:        fact.ID,
			Kind:      fact.Kind,
			Subject:   fact.Subject,
			Predicate: fact.Predicate,
			Object:    fact.Object,
			Entities:  append([]string(nil), fact.Entities...),
			ValidFrom: fact.ValidFrom,
			Snippet:   compactSnippet(fact.Content, 180),
		})
	}
	rec.Flags = extractQualityFlags(rec)
	rec.Status = extractQualityStatus(rec)
	return rec
}

func extractKindCounts(facts []factDumpFact) map[string]int {
	if len(facts) == 0 {
		return nil
	}
	out := map[string]int{}
	for _, fact := range facts {
		if fact.Kind != "" {
			out[fact.Kind]++
		}
	}
	return out
}

func extractSlotCoverageForFacts(facts []factDumpFact) extractSlotCoverage {
	if len(facts) == 0 {
		return extractSlotCoverage{}
	}
	total := float64(len(facts))
	var kind, subject, predicate, object, entities, validFrom int
	for _, fact := range facts {
		if fact.Kind != "" {
			kind++
		}
		if fact.Subject != "" {
			subject++
		}
		if fact.Predicate != "" {
			predicate++
		}
		if fact.Object != "" {
			object++
		}
		if len(fact.Entities) > 0 {
			entities++
		}
		if fact.ValidFrom != "" {
			validFrom++
		}
	}
	return extractSlotCoverage{
		Kind:      float64(kind) / total,
		Subject:   float64(subject) / total,
		Predicate: float64(predicate) / total,
		Object:    float64(object) / total,
		Entities:  float64(entities) / total,
		ValidFrom: float64(validFrom) / total,
	}
}

func extractQualityFlags(rec extractQualityRecord) []string {
	var flags []string
	if rec.FactsCount == 0 {
		return []string{"no_facts_for_evidence"}
	}
	if rec.TermCoverage < 0.35 {
		flags = append(flags, "low_term_coverage")
	}
	if rec.TermCoverage < 0.65 {
		flags = append(flags, "partial_term_coverage")
	}
	if rec.SlotCoverage.Subject < 0.5 {
		flags = append(flags, "subject_sparse")
	}
	if rec.SlotCoverage.ValidFrom < 0.5 {
		flags = append(flags, "valid_from_sparse")
	}
	if rec.CompoundFacts > 0 {
		flags = append(flags, "possible_compound_fact")
	}
	if rec.FactsCount == 1 && len(rec.ExpectedTerms) >= 10 && rec.TermCoverage < 0.65 {
		flags = append(flags, "possible_over_abstracted")
	}
	return uniqueStrings(flags)
}

func extractQualityStatus(rec extractQualityRecord) string {
	if rec.FactsCount == 0 {
		return "extract_miss"
	}
	if rec.TermCoverage < 0.35 {
		return "semantic_drift"
	}
	if rec.TermCoverage < 0.65 {
		return "partial_coverage"
	}
	if rec.CompoundFacts > 0 {
		return "needs_review"
	}
	return "ok"
}

func looksCompoundFact(content string) bool {
	s := strings.ToLower(content)
	if len(s) > 220 {
		return true
	}
	if strings.Count(s, ";") >= 1 {
		return true
	}
	if strings.Count(s, ",") >= 3 {
		return true
	}
	if strings.Count(s, " and ") >= 2 {
		return true
	}
	return false
}

func writeExtractQualityMarkdown(w io.Writer, records []extractQualityRecord) {
	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	writeExtractCountTable(w, "status", countExtractField(records, func(rec extractQualityRecord) string { return rec.Status }))
	writeExtractCountTable(w, "flag", countExtractFlags(records))
	fmt.Fprintln(w, "## Evidence Turns")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| evidence_id | status | coverage | facts | flags | questions | text |")
	fmt.Fprintln(w, "|---|---|---:|---:|---|---|---|")
	for _, rec := range records {
		if rec.Status == "ok" {
			continue
		}
		fmt.Fprintf(w, "| %s | %s | %.2f | %d | %s | %s | %s |\n",
			rec.EvidenceID, rec.Status, rec.TermCoverage, rec.FactsCount,
			strings.Join(rec.Flags, ","), strings.Join(rec.QuestionIDs, ","),
			compactSnippet(strings.ReplaceAll(rec.TextSnippet, "|", "\\|"), 120))
	}
}

func writeExtractCountTable(w io.Writer, name string, counts map[string]int) {
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

func countExtractField(records []extractQualityRecord, pick func(extractQualityRecord) string) map[string]int {
	out := map[string]int{}
	for _, rec := range records {
		if key := pick(rec); key != "" {
			out[key]++
		}
	}
	return out
}

func countExtractFlags(records []extractQualityRecord) map[string]int {
	out := map[string]int{}
	for _, rec := range records {
		for _, flag := range rec.Flags {
			out[flag]++
		}
	}
	return out
}
