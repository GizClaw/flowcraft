package workspace

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	evidencestore "github.com/GizClaw/flowcraft/memory/recall/internal/store/evidence"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type evidenceStore struct {
	b *Backend
}

func (s *evidenceStore) Append(ctx context.Context, scope domain.Scope, factID string, refs []domain.EvidenceRef) error {
	if scope.RuntimeID == "" {
		return errdefs.Validationf("recall workspace evidence: scope.runtime_id is required")
	}
	if factID == "" {
		return errdefs.Validationf("recall workspace evidence: fact id is required")
	}
	if len(refs) == 0 {
		return nil
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return err
	}
	for i, ref := range refs {
		if ref.ID == "" {
			ref.ID = fmt.Sprintf("%s#%d", factID, i)
		}
		rec := evidenceRecord{
			Scope:      scopeFromPartition(scope),
			FactID:     factID,
			EvidenceID: ref.ID,
			Ordinal:    i,
			Ref:        ref,
		}
		idx := evidenceFactIndex(st.Evidence, scope, factID, ref.ID)
		if idx >= 0 {
			if st.Evidence[idx].Ordinal != rec.Ordinal || !reflect.DeepEqual(st.Evidence[idx].Ref, rec.Ref) {
				return errdefs.Conflictf("recall workspace evidence: duplicate evidence id %q for fact %q with different payload", ref.ID, factID)
			}
			continue
		}
		st.Evidence = append(st.Evidence, rec)
	}
	return s.b.save(ctx, st)
}

func (s *evidenceStore) Get(ctx context.Context, scope domain.Scope, evidenceID string) (domain.EvidenceRef, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return domain.EvidenceRef{}, err
	}
	records := evidenceRecordsByID(st.Evidence, scope, evidenceID)
	if len(records) == 0 {
		return domain.EvidenceRef{}, evidencestore.ErrNotFound
	}
	if len(records) > 1 {
		return domain.EvidenceRef{}, evidencestore.ErrAmbiguous
	}
	return records[0].Ref, nil
}

func (s *evidenceStore) ListByFact(ctx context.Context, scope domain.Scope, factID string) ([]domain.EvidenceRef, error) {
	if factID == "" {
		return nil, nil
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return nil, err
	}
	records := make([]evidenceRecord, 0)
	for _, rec := range st.Evidence {
		if samePartition(rec.Scope, scope) && rec.FactID == factID {
			records = append(records, rec)
		}
	}
	sortEvidenceRecords(records)
	out := make([]domain.EvidenceRef, 0, len(records))
	for _, rec := range records {
		out = append(out, rec.Ref)
	}
	return out, nil
}

func (s *evidenceStore) ListFactIDs(ctx context.Context, scope domain.Scope) ([]string, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, rec := range st.Evidence {
		if samePartition(rec.Scope, scope) && rec.FactID != "" {
			seen[rec.FactID] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func (s *evidenceStore) ForgetByFact(ctx context.Context, scope domain.Scope, factIDs []string) error {
	ids := sqlstmt.UniqueNonEmpty(factIDs)
	if len(ids) == 0 {
		return nil
	}
	targets := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		targets[id] = struct{}{}
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return err
	}
	filtered := st.Evidence[:0]
	for _, rec := range st.Evidence {
		if samePartition(rec.Scope, scope) {
			if _, ok := targets[rec.FactID]; ok {
				continue
			}
		}
		filtered = append(filtered, rec)
	}
	st.Evidence = filtered
	return s.b.save(ctx, st)
}

func (s *evidenceStore) Close() error { return nil }

func evidenceRecordsByID(records []evidenceRecord, scope domain.Scope, evidenceID string) []evidenceRecord {
	var out []evidenceRecord
	for _, rec := range records {
		if samePartition(rec.Scope, scope) && rec.EvidenceID == evidenceID {
			out = append(out, rec)
		}
	}
	return out
}

func evidenceFactIndex(records []evidenceRecord, scope domain.Scope, factID, evidenceID string) int {
	for i, rec := range records {
		if samePartition(rec.Scope, scope) && rec.FactID == factID && rec.EvidenceID == evidenceID {
			return i
		}
	}
	return -1
}

func sortEvidenceRecords(records []evidenceRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Ordinal == records[j].Ordinal {
			return records[i].EvidenceID < records[j].EvidenceID
		}
		return records[i].Ordinal < records[j].Ordinal
	})
}
