// Package revision owns the pipeline that drives Memory.Fork and
// Memory.Contest (Cluster A 2026-05-21).
//
// Both APIs share the same three-stage shape — lookup the source
// fact, attach a Revision annotation to the new fact, then run it
// through the canonical write pipeline — so a single State + Runner
// captures both flavours via the Mode field.
package revision

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// Mode discriminates the revision flavour the runner is executing.
//
//   - ModeFork: parallel branch. The source fact stays canonical-
//     active; the new fact carries Revision{Kind: RevisionFork}.
//   - ModeContest: challenge note. A new note-shaped fact is
//     constructed inside attach_revision (callers do not supply one)
//     carrying Revision{Kind: RevisionContest} plus the supplied
//     evidence refs.
type Mode int

const (
	// ModeFork starts at 1 so the zero value cannot accidentally
	// pass for "Fork" — callers MUST pick one explicitly.
	ModeFork Mode = iota + 1
	ModeContest
)

// State is the per-call workspace threaded through the revision
// pipeline. The facade (Memory.Fork / Memory.Contest) populates
// Scope / Mode / SourceFactID and either NewFact (Fork) or
// Note + Evidence (Contest); stages fill in Source / Created.
type State struct {
	// Inputs.

	Scope        domain.Scope
	Mode         Mode
	SourceFactID string

	// NewFact is the caller-supplied draft used by ModeFork. The
	// attach stage stamps Scope and Revision onto it before save.
	NewFact domain.TemporalFact

	// Note is the human-readable challenge body used by
	// ModeContest. attach_revision wraps it in a FactNote when
	// constructing the contest fact.
	Note string

	// Evidence is the optional EvidenceRefs slice (Contest path).
	// Fork callers attach evidence to NewFact directly.
	Evidence []domain.EvidenceRef

	// Outputs.

	// Source is the looked-up canonical source fact.
	Source domain.TemporalFact

	// Created is the freshly-saved revision fact (after the save
	// stage commits). Facade returns this back to the caller.
	Created domain.TemporalFact

	// Trace mirrors the other pipeline trace shapes. nil = caller
	// did not request explain output (zero allocation).
	Trace *Trace
}

// Trace carries the per-stage diagnostics. Kept local because no
// public API currently returns it; lift to a domain-owned trace
// when a public Memory.ForkExplain / ContestExplain lands.
type Trace struct {
	Stages []diagnostic.StageDiagnostic
}

// EnsureTrace allocates the Trace if not pre-populated. Idempotent.
func (s *State) EnsureTrace() *Trace {
	if s.Trace == nil {
		s.Trace = &Trace{}
	}
	return s.Trace
}

// AppendStage is the TraceAppender registered with the pipeline
// framework.
func (s *State) AppendStage(d diagnostic.StageDiagnostic) {
	if s.Trace == nil {
		return
	}
	s.Trace.Stages = append(s.Trace.Stages, d)
}

// KindString renders the Mode as the canonical RevisionKind string
// for diagnostic emission. Returns "" on an unrecognised Mode so
// stage authors can stamp it on a detail without a switch.
func (m Mode) KindString() string {
	switch m {
	case ModeFork:
		return string(domain.RevisionFork)
	case ModeContest:
		return string(domain.RevisionContest)
	}
	return ""
}
