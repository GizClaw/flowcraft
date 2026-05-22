package flowcraft

import (
	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	recallv1 "github.com/GizClaw/flowcraft/sdk/recall"
)

func toRecallV1Scope(s runners.Scope) recallv1.Scope {
	return recallv1.Scope{
		RuntimeID: s.RuntimeID,
		UserID:    s.UserID,
		AgentID:   s.AgentID,
	}
}

func fromRecallV1Hit(h recallv1.Hit) runners.Hit {
	hit := runners.Hit{
		ID:      h.Entry.ID,
		Content: h.Entry.Content,
		Score:   h.Score,
	}
	if len(h.Entry.Categories) > 0 || len(h.Scores) > 0 {
		hit.Metadata = make(map[string]any, 2)
		if len(h.Entry.Categories) > 0 {
			hit.Metadata["categories"] = append([]string(nil), h.Entry.Categories...)
		}
		if len(h.Scores) > 0 {
			scores := make(map[string]float64, len(h.Scores))
			for k, v := range h.Scores {
				scores[k] = v
			}
			hit.Metadata["scores"] = scores
		}
	}
	return hit
}

func fromRecallV1Hits(hits []recallv1.Hit) []runners.Hit {
	if len(hits) == 0 {
		return nil
	}
	out := make([]runners.Hit, len(hits))
	for i, h := range hits {
		out[i] = fromRecallV1Hit(h)
	}
	return out
}
