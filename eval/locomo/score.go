package locomo

import "github.com/GizClaw/flowcraft/eval/locomo/runners"

// aggregateByCategory groups per-question scores by their canonical
// category tag and returns the mean of each headline metric per group.
// Only canonical labels (see convCategoryName in cli_convert.go) are
// emitted as keys — the raw `catN` tags are filtered out so the report
// surface stays stable even if the upstream JSON renumbers categories.
// Questions without a canonical tag are skipped silently rather than
// bucketed into a synthetic "unknown" group: a missing breakdown is
// less misleading than a wrong one.
func aggregateByCategory(scores []QuestionScore) map[string]CategoryScore {
	canonical := map[string]bool{
		"single-hop":  true,
		"temporal":    true,
		"multi-hop":   true,
		"open-domain": true,
		"adversarial": true,
	}
	type acc struct {
		n                  int
		sumEM, sumF1, sumJ float64
	}
	groups := map[string]*acc{}
	for _, s := range scores {
		for _, tag := range s.Tags {
			if !canonical[tag] {
				continue
			}
			g, ok := groups[tag]
			if !ok {
				g = &acc{}
				groups[tag] = g
			}
			g.n++
			g.sumEM += s.EM
			g.sumF1 += s.F1
			g.sumJ += s.Judge
		}
	}
	if len(groups) == 0 {
		return nil
	}
	out := make(map[string]CategoryScore, len(groups))
	for tag, g := range groups {
		if g.n == 0 {
			continue
		}
		out[tag] = CategoryScore{
			Count: g.n,
			EM:    g.sumEM / float64(g.n),
			F1:    g.sumF1 / float64(g.n),
			Judge: g.sumJ / float64(g.n),
		}
	}
	return out
}

func evidenceKHit(artifacts []runners.RecallArtifact, want []string) float64 {
	if len(want) == 0 {
		return 0
	}
	got := map[string]struct{}{}
	for _, artifact := range artifacts {
		if artifact.ID != "" {
			got[artifact.ID] = struct{}{}
		}
		for _, eid := range artifact.EvidenceIDs {
			got[eid] = struct{}{}
		}
	}
	for _, w := range want {
		if _, ok := got[w]; ok {
			return 1
		}
	}
	return 0
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
