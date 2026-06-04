package planner_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/lens"
	entitylens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/entity"
	graphlens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/graph"
	profilelens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/profile"
	relationlens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/relation"
	retrievallens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/retrieval"
	semanticlens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/semantic"
	timelinelens "github.com/GizClaw/flowcraft/memory/recall/internal/lens/timeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// TestLensRegistryOrderMatchesBuiltin verifies lens registration produces the
// same SourceOrder as the builtin planner.
func TestLensRegistryOrderMatchesBuiltin(t *testing.T) {
	reg := lens.NewRegistry()
	// mirror memory.New registration order (graph on, no evidence)
	reg.Register(retrievallens.Lens{})
	reg.Register(entitylens.Lens{})
	reg.Register(graphlens.Lens{})
	reg.Register(relationlens.Lens{})
	reg.Register(semanticlens.AssertionLens())
	reg.Register(profilelens.Lens{})
	reg.Register(timelinelens.Lens{})
	specs := reg.Specs()
	newPlanner := planner.NewFromSpecs(specs)
	builtin := planner.New()
	scope := domain.Scope{RuntimeID: "rt"}

	cases := []struct {
		name  string
		input port.PlannerInput
	}{
		{"retrieval only", port.PlannerInput{Scope: scope, Text: "hello", GraphEnabled: true}},
		{"with entities", port.PlannerInput{Scope: scope, Entities: []string{"alice"}, GraphEnabled: true}},
		{"relation hints", port.PlannerInput{Scope: scope, Subject: "alice", Predicate: "likes", GraphEnabled: true}},
		{"profile subject", port.PlannerInput{Scope: scope, Subject: "alice", GraphEnabled: true}},
		{"timeline range", port.PlannerInput{
			Scope: scope, GraphEnabled: true,
			TimeRange: domain.TimeRange{
				From: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
				To:   time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		}},
		{"graph", port.PlannerInput{Scope: scope, Entities: []string{"alice"}, GraphEnabled: true}},
	}
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newPlanner.Plan(ctx, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			want, err := builtin.Plan(ctx, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.SourceOrder) != len(want.SourceOrder) {
				t.Fatalf("order len %d vs builtin %d: got %v want %v", len(got.SourceOrder), len(want.SourceOrder), got.SourceOrder, want.SourceOrder)
			}
			for i := range got.SourceOrder {
				if got.SourceOrder[i] != want.SourceOrder[i] {
					t.Fatalf("index %d: got %q want %q (full got=%v want=%v)", i, got.SourceOrder[i], want.SourceOrder[i], got.SourceOrder, want.SourceOrder)
				}
			}
		})
	}
}
