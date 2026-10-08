// Package verify runs the read-only integrity inspection behind
// Assembly.Verify: dangling fact links, source/view digest drift, summary
// input and digest mismatches, and projection digest drift. It never mutates
// state; repair execution lives outside the library.
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const AlgorithmVersion = "verify-v1"

type ActionKind string

const (
	ActionReplay     ActionKind = "replay"
	ActionQuarantine ActionKind = "quarantine"
	ActionRebuild    ActionKind = "rebuild"
)

type Action struct {
	Kind     ActionKind `json:"kind"`
	Target   string     `json:"target"`
	Evidence string     `json:"evidence"`
}

// Plan is the read-only verdict for one scope: the actions an operator or a
// host-side maintenance job would need to take.
type Plan struct {
	ID               string           `json:"id"`
	Scope            corememory.Scope `json:"scope"`
	Actions          []Action         `json:"actions"`
	AlgorithmVersion string           `json:"algorithm_version"`
}

type FactEvidence struct {
	ID        string
	LinkedIDs []string
}

type SummaryInputKind string

const (
	SummaryInputFact    SummaryInputKind = "fact"
	SummaryInputSummary SummaryInputKind = "summary"
)

type SummaryEvidence struct {
	ID                                 string
	Level                              uint8
	InputKind                          SummaryInputKind
	InputIDs                           []string
	CoverageValid                      bool
	SourceDigest, ComputedSourceDigest string
}

type SourceViewEvidence struct{ Name, SourceDigest, ViewDigest string }

type ProjectionEvidence struct {
	Name                                     string
	StoredSourceDigest, ComputedSourceDigest string
	StoredBuildDigest, ComputedBuildDigest   string
}

type Input struct {
	Facts       []FactEvidence
	Summaries   []SummaryEvidence
	Sources     []SourceViewEvidence
	Projections []ProjectionEvidence
}

// Inspect runs the inspection with a background context.
func Inspect(scope corememory.Scope, input Input) (Plan, error) {
	return InspectContext(context.Background(), scope, input)
}

// InspectContext validates the evidence and returns the actions required to
// restore consistency. A healthy scope yields an empty action list.
func InspectContext(ctx context.Context, scope corememory.Scope, input Input) (Plan, error) {
	if err := scope.Validate(); err != nil {
		return Plan{}, err
	}
	factIDs := map[string]struct{}{}
	for _, value := range input.Facts {
		factIDs[value.ID] = struct{}{}
	}
	var actions []Action
	for _, value := range input.Facts {
		for _, linked := range value.LinkedIDs {
			if _, exists := factIDs[linked]; !exists {
				actions = append(actions, Action{ActionReplay, "fact:" + value.ID, "dangling linked id:" + linked})
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	summaryIDs := make(map[string]struct{}, len(input.Summaries))
	for _, value := range input.Summaries {
		summaryIDs[value.ID] = struct{}{}
	}
	for _, value := range input.Summaries {
		invalidInputs := len(value.InputIDs) == 0 || value.Level > 3
		expectedKind := SummaryInputSummary
		if value.Level == 0 {
			expectedKind = SummaryInputFact
		}
		if value.InputKind != expectedKind {
			invalidInputs = true
		}
		for _, id := range value.InputIDs {
			if id == value.ID {
				invalidInputs = true
				break
			}
			var exists bool
			if value.Level == 0 {
				_, exists = factIDs[id]
			} else {
				_, exists = summaryIDs[id]
			}
			if !exists {
				invalidInputs = true
				break
			}
		}
		if invalidInputs || !value.CoverageValid || value.SourceDigest != value.ComputedSourceDigest {
			actions = append(actions, Action{ActionRebuild, "summary:" + value.ID, "input/coverage/source digest mismatch"})
		}
	}
	for _, value := range input.Sources {
		if value.SourceDigest != value.ViewDigest {
			actions = append(actions, Action{ActionReplay, "view:" + value.Name, "source/view digest mismatch"})
		}
	}
	for _, value := range input.Projections {
		if value.StoredBuildDigest != value.ComputedBuildDigest ||
			value.StoredSourceDigest != value.ComputedSourceDigest {
			actions = append(actions, Action{ActionRebuild, "projection:" + value.Name, "build/source digest mismatch"})
		}
	}
	sort.Slice(actions, func(i, j int) bool {
		if actions[i].Target != actions[j].Target {
			return actions[i].Target < actions[j].Target
		}
		return actions[i].Evidence < actions[j].Evidence
	})
	payload, _ := json.Marshal(struct {
		Scope   corememory.Scope
		Actions []Action
		Version string
	}{scope, actions, AlgorithmVersion})
	return Plan{ID: digest("verify-plan", payload), Scope: scope, Actions: actions, AlgorithmVersion: AlgorithmVersion}, nil
}

func digest(domain string, value []byte) string {
	sum := sha256.Sum256(append([]byte("flowcraft.memory."+domain+"\x00v1\x00"), value...))
	return hex.EncodeToString(sum[:])
}
