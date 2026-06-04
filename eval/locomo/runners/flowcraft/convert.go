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

func fromRecallV1Artifact(h recallv1.Hit) runners.RecallArtifact {
	artifact := runners.RecallArtifact{
		ID:         h.Entry.ID,
		Content:    h.Entry.Content,
		ScoreLabel: "final_score",
		FinalScore: h.Score,
	}
	if len(h.Entry.Categories) > 0 || len(h.Scores) > 0 {
		artifact.Metadata = make(map[string]any, 2)
		if len(h.Entry.Categories) > 0 {
			artifact.Metadata["categories"] = append([]string(nil), h.Entry.Categories...)
		}
		if len(h.Scores) > 0 {
			scores := make(map[string]float64, len(h.Scores))
			for k, v := range h.Scores {
				scores[k] = v
			}
			artifact.Metadata["scores"] = scores
		}
	}
	return artifact
}

func fromRecallV1Artifacts(hits []recallv1.Hit) []runners.RecallArtifact {
	if len(hits) == 0 {
		return nil
	}
	out := make([]runners.RecallArtifact, len(hits))
	for i, h := range hits {
		out[i] = fromRecallV1Artifact(h)
	}
	return out
}
