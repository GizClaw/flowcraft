package domain

// RevisionKind classifies how a write relates to prior facts.
//
//   - RevisionSupersede: default merge_key conflict path (close prior).
//   - RevisionFork: parallel branch; prior stays active.
//   - RevisionContest: challenge an existing fact with new evidence.
//
// Merge (same merge_key value change) uses RevisionSupersede via the
// resolver. Retract is not a revision kind — use Forget(Soft) (D.8).
type RevisionKind string

const (
	RevisionSupersede RevisionKind = "supersede"
	RevisionFork      RevisionKind = "fork"
	RevisionContest   RevisionKind = "contest"
)

// Revision annotates a fact for conflict resolution. Callers set it on
// TemporalFact.Metadata via AttachRevision; the ingest resolver reads
// it back with RevisionOf.
type Revision struct {
	Kind         RevisionKind
	SourceFactID string
}

// AttachRevision stores a revision annotation on f.Metadata. It does
// not mutate unrelated keys.
func AttachRevision(f *TemporalFact, rev Revision) {
	if f == nil || rev.Kind == "" {
		return
	}
	if f.Metadata == nil {
		f.Metadata = make(map[string]any)
	}
	f.Metadata[MetaRevisionKind] = string(rev.Kind)
	if rev.SourceFactID != "" {
		switch rev.Kind {
		case RevisionFork:
			f.Metadata[MetaForkOf] = rev.SourceFactID
		case RevisionContest:
			f.Metadata[MetaContestOf] = rev.SourceFactID
		}
	}
}

// RevisionOf reads a revision annotation from fact metadata.
func RevisionOf(f TemporalFact) (Revision, bool) {
	if f.Metadata == nil {
		return Revision{}, false
	}
	raw, _ := f.Metadata[MetaRevisionKind].(string)
	k := RevisionKind(raw)
	if k == "" {
		return Revision{}, false
	}
	rev := Revision{Kind: k}
	if id, _ := f.Metadata[MetaForkOf].(string); id != "" {
		rev.SourceFactID = id
	} else if id, _ := f.Metadata[MetaContestOf].(string); id != "" {
		rev.SourceFactID = id
	}
	return rev, true
}
