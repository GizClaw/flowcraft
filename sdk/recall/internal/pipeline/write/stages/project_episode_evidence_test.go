package stages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// fakeEpisodeProjection accepts KindEpisode and records every call so
// project_episode_evidence tests can assert routing + compensation
// without standing up the full lens/evidence stack.
type fakeEpisodeProjection struct {
	name      string
	accepts   bool
	projectIn []domain.TemporalFact
	projectEr error
	forgotten []string
	forgetErr error
}

func (p *fakeEpisodeProjection) Name() string                  { return p.name }
func (p *fakeEpisodeProjection) Consistency() port.Consistency { return port.Required }
func (p *fakeEpisodeProjection) Project(_ context.Context, facts []domain.TemporalFact) error {
	if p.projectEr != nil {
		return p.projectEr
	}
	p.projectIn = append(p.projectIn, facts...)
	return nil
}

func (p *fakeEpisodeProjection) Forget(_ context.Context, _ domain.Scope, ids []string) error {
	p.forgotten = append(p.forgotten, ids...)
	return p.forgetErr
}

func (p *fakeEpisodeProjection) Rebuild(context.Context, domain.Scope, []domain.TemporalFact) error {
	return nil
}
func (p *fakeEpisodeProjection) ClearScope(context.Context, domain.Scope) error { return nil }
func (p *fakeEpisodeProjection) AcceptsKind(k domain.FactKind) bool {
	if p.accepts {
		return true
	}
	return k != domain.KindEpisode
}

func TestProjectEpisodeEvidence_SkipsUnfilteredRequiredProjection(t *testing.T) {
	plain := &fakeProjection{name: "plain", level: port.Required}
	accepting := &fakeEpisodeProjection{name: "evidence", accepts: true}
	fanout := pipeline.NewFanout([]port.Projection{plain, accepting}, nil)
	s := stages.NewProjectEpisodeEvidence(fanout, []port.Projection{plain, accepting}, nil)
	state := &write.WriteState{
		Scope:        domain.Scope{RuntimeID: "rt"},
		EpisodeFacts: []domain.TemporalFact{{ID: "epi-1", Kind: domain.KindEpisode}},
	}
	if _, err := s.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if plain.projectCalls != 0 {
		t.Errorf("unfiltered projection projectCalls = %d, want 0", plain.projectCalls)
	}
	if len(accepting.projectIn) != 1 {
		t.Errorf("accepting projection received %d facts, want 1", len(accepting.projectIn))
	}
}

type fakeProjection struct {
	name         string
	level        port.Consistency
	projectCalls int
}

func (p *fakeProjection) Name() string                  { return p.name }
func (p *fakeProjection) Consistency() port.Consistency { return p.level }
func (p *fakeProjection) Project(context.Context, []domain.TemporalFact) error {
	p.projectCalls++
	return nil
}
func (p *fakeProjection) Forget(context.Context, domain.Scope, []string) error { return nil }
func (p *fakeProjection) Rebuild(context.Context, domain.Scope, []domain.TemporalFact) error {
	return nil
}
func (p *fakeProjection) ClearScope(context.Context, domain.Scope) error { return nil }

func TestProjectEpisodeEvidence_HappyPathRoutesOnlyToAccepting(t *testing.T) {
	accepting := &fakeEpisodeProjection{name: "evidence", accepts: true}
	rejecting := &fakeEpisodeProjection{name: "retrieval", accepts: false}
	fanout := pipeline.NewFanout([]port.Projection{accepting, rejecting}, nil)
	s := stages.NewProjectEpisodeEvidence(fanout, []port.Projection{accepting, rejecting}, nil)
	state := &write.WriteState{
		Scope:        domain.Scope{RuntimeID: "rt"},
		EpisodeFacts: []domain.TemporalFact{{ID: "epi-1", Kind: domain.KindEpisode}},
	}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !state.EvidenceAppliedEpisode {
		t.Errorf("EvidenceAppliedEpisode = false")
	}
	if len(accepting.projectIn) != 1 {
		t.Errorf("accepting projection received %d facts, want 1", len(accepting.projectIn))
	}
	if len(rejecting.projectIn) != 0 {
		t.Errorf("rejecting projection received %d facts, want 0", len(rejecting.projectIn))
	}
	detail, ok := d.(diagnostic.ProjectEpisodeEvidenceDetail)
	if !ok {
		t.Fatalf("Detail type = %T", d)
	}
	if detail.EpisodeFacts != 1 {
		t.Errorf("Detail.EpisodeFacts = %d", detail.EpisodeFacts)
	}
}

func TestProjectEpisodeEvidence_FanoutErrPropagates(t *testing.T) {
	boom := errors.New("evidence down")
	accepting := &fakeEpisodeProjection{name: "evidence", accepts: true, projectEr: boom}
	fanout := pipeline.NewFanout([]port.Projection{accepting}, nil)
	s := stages.NewProjectEpisodeEvidence(fanout, []port.Projection{accepting}, nil)
	state := &write.WriteState{
		Scope:        domain.Scope{RuntimeID: "rt"},
		EpisodeFacts: []domain.TemporalFact{{ID: "epi-1", Kind: domain.KindEpisode}},
	}
	_, err := s.Run(context.Background(), state)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if state.FailedStage != "project_episode_evidence" {
		t.Errorf("FailedStage = %q", state.FailedStage)
	}
	if state.EvidenceAppliedEpisode {
		t.Errorf("EvidenceAppliedEpisode must stay false on failure")
	}
}

func TestProjectEpisodeEvidence_CompensateForgetsAcceptingProjections(t *testing.T) {
	accepting := &fakeEpisodeProjection{name: "evidence", accepts: true}
	rejecting := &fakeEpisodeProjection{name: "retrieval", accepts: false}
	s := stages.NewProjectEpisodeEvidence(nil, []port.Projection{accepting, rejecting}, nil)
	state := &write.WriteState{
		Scope:                  domain.Scope{RuntimeID: "rt"},
		EpisodeFacts:           []domain.TemporalFact{{ID: "epi-1"}, {ID: "epi-2"}},
		EvidenceAppliedEpisode: true,
		FailedStage:            "write_semantic_outbox",
	}
	if err := s.Compensate(context.Background(), state); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if len(accepting.forgotten) != 2 {
		t.Errorf("accepting projection forgotten = %v, want 2 ids", accepting.forgotten)
	}
	if len(rejecting.forgotten) != 0 {
		t.Errorf("rejecting projection forgotten = %v, want none", rejecting.forgotten)
	}
}

func TestProjectEpisodeEvidence_CompensateEmitsTelemetryOnForgetFailure(t *testing.T) {
	boom := errors.New("evidence forget down")
	accepting := &fakeEpisodeProjection{name: "evidence", accepts: true, forgetErr: boom}
	hook := &recordHook{}
	s := stages.NewProjectEpisodeEvidence(nil, []port.Projection{accepting}, hook)
	state := &write.WriteState{
		Scope:                  domain.Scope{RuntimeID: "rt"},
		EpisodeFacts:           []domain.TemporalFact{{ID: "epi-1"}},
		EvidenceAppliedEpisode: true,
		FailedStage:            "write_semantic_outbox",
	}
	if err := s.Compensate(context.Background(), state); err != nil {
		t.Fatalf("Compensate (best-effort) must not return err: %v", err)
	}
	if len(hook.events) != 1 {
		t.Fatalf("hook events = %d", len(hook.events))
	}
	d, ok := hook.events[0].Detail.(diagnostic.CompensationFailedDetail)
	if !ok {
		t.Fatalf("Detail type = %T", hook.events[0].Detail)
	}
	if d.OriginalStage != "save_rollback.episode_evidence:evidence" {
		t.Errorf("OriginalStage = %q", d.OriginalStage)
	}
}

func TestProjectEpisodeEvidence_CompensateSkipsWhenNotApplied(t *testing.T) {
	accepting := &fakeEpisodeProjection{name: "evidence", accepts: true}
	s := stages.NewProjectEpisodeEvidence(nil, []port.Projection{accepting}, nil)
	state := &write.WriteState{
		Scope:        domain.Scope{RuntimeID: "rt"},
		EpisodeFacts: []domain.TemporalFact{{ID: "epi-1"}},
	}
	if err := s.Compensate(context.Background(), state); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if len(accepting.forgotten) != 0 {
		t.Errorf("Compensate must skip Forget when EvidenceAppliedEpisode=false")
	}
}
