package read

// PromoteMergedItems copies the primary sub-scope's materialized
// items into MergedItems when federation_merge is not in play (the
// Phase B.3 single-scope fast path). Idempotent when MergedItems is
// already populated.
func PromoteMergedItems(s *ReadState) {
	if s == nil || len(s.MergedItems) > 0 {
		return
	}
	sub := s.PrimarySubScope()
	if sub == nil {
		return
	}
	s.MergedItems = sub.Materialized
}
