package telemetry

import "github.com/GizClaw/flowcraft/sdk/recall/internal/model"

// AttributeRecallTrace converts a RecallTrace into diagnostics
// attributions. It never mutates the trace.
func AttributeRecallTrace(trace model.RecallTrace) []Attribution {
	var out []Attribution
	for _, st := range trace.Sources {
		if st.Err == "" {
			continue
		}
		out = append(out, Attribution{
			Stage:   StageSource,
			Source:  st.Source,
			Reason:  "source_error",
			Details: st.Err,
		})
	}
	for _, d := range trace.Drops {
		out = append(out, Attribution{
			Stage:   StageFromDropReason(d.Reason),
			FactID:  d.FactID,
			Source:  d.Source,
			Reason:  string(d.Reason),
			Details: d.Details,
		})
	}
	if trace.FusedCandidates > 0 && trace.Materialized == 0 && len(trace.Drops) == 0 {
		out = append(out, Attribution{
			Stage:  StageMaterialize,
			Reason: "zero_materialized",
		})
	}
	return out
}

// AttributeDroppedFacts maps write-path compiler drops to stages.
func AttributeDroppedFacts(drops []DroppedFact) []Attribution {
	if len(drops) == 0 {
		return nil
	}
	out := make([]Attribution, 0, len(drops))
	for _, d := range drops {
		stage := StageExtract
		if len(d.Reason) >= 11 && d.Reason[:11] == "governance:" {
			stage = StageNormalize
		} else if len(d.Reason) >= 7 && d.Reason[:7] == "policy:" {
			stage = StageNormalize
		}
		out = append(out, Attribution{
			Stage:   stage,
			FactID:  d.Fact.ID,
			Reason:  d.Reason,
			Details: d.Fact.Content,
		})
	}
	return out
}

// DroppedFact mirrors compiler.DroppedFact without importing compiler.
type DroppedFact struct {
	Fact   model.TemporalFact
	Reason string
}
