package domain

import "time"

// EvidenceRow is the answer-facing structured view of grounded recall hits.
// It lets an answer layer render a table and run deterministic checks before
// handing evidence to an LLM for wording.
type EvidenceRow struct {
	FactID     string
	EvidenceID string
	Subject    string
	Predicate  string
	Object     string
	Time       *time.Time
	Quote      string
}

// ReasoningResult is a deterministic summary for task-aware answer layers.
type ReasoningResult struct {
	Outcome  string
	Evidence int
}

func BuildEvidenceTable(hits []Hit) []EvidenceRow {
	var rows []EvidenceRow
	for _, hit := range hits {
		f := hit.Fact
		refs := hit.Evidence
		if len(refs) == 0 {
			rows = append(rows, evidenceRowFromFact(f, EvidenceRef{}))
			continue
		}
		for _, ref := range refs {
			rows = append(rows, evidenceRowFromFact(f, ref))
		}
	}
	return rows
}

func ReasonEvidence(tasks []QueryTaskIntent, rows []EvidenceRow) ReasoningResult {
	res := ReasoningResult{Outcome: "evidence", Evidence: len(rows)}
	for _, task := range tasks {
		switch task {
		case QueryTaskYesNoVerification, QueryTaskAbsenceCheck:
			if len(rows) == 0 {
				res.Outcome = "unknown"
			} else {
				res.Outcome = "evidence"
			}
		case QueryTaskTemporalReasoning:
			res.Outcome = "temporal"
		}
	}
	if len(rows) == 0 {
		res.Outcome = "unknown"
	}
	return res
}

func evidenceRowFromFact(f TemporalFact, ref EvidenceRef) EvidenceRow {
	row := EvidenceRow{
		FactID:     f.ID,
		EvidenceID: firstNonEmptyString(ref.ID, ref.MessageID),
		Subject:    f.Subject,
		Predicate:  f.Predicate,
		Object:     f.Object,
		Quote:      firstNonEmptyString(ref.Text, f.EvidenceText, f.Content),
	}
	if f.ValidFrom != nil && !f.ValidFrom.IsZero() {
		t := *f.ValidFrom
		row.Time = &t
	} else if !f.ObservedAt.IsZero() {
		t := f.ObservedAt
		row.Time = &t
	}
	return row
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
