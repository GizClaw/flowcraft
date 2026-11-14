package delegation

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
)

func (s *Service) startAsync(
	ctx context.Context, req Request, run runctx.Run, callID string,
	call Call, childSession string,
) (Outcome, error) {
	now := time.Now()
	job := Job{
		ID:              childSession,
		Target:          req.Target,
		Mode:            ModeAsync,
		Status:          StatusAccepted,
		Input:           req.Input,
		ParentSessionID: run.SessionID,
		ParentAgent:     run.AgentName,
		ParentTaskID:    run.TaskID,
		CallID:          callID,
		Depth:           call.Depth,
		ChainID:         call.ChainID,
		ChildSessionID:  childSession,
		// The job runs after the call that started it returns, so the parent's
		// span context travels with the job instead of with a context.
		TraceParent: traceParent(ctx),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	created, found, err := s.store.Create(ctx, job)
	if err != nil {
		return Outcome{}, err
	}
	if found {
		return outcomeOf(created), nil
	}

	// A child that already finished (or paused) is read back, not run again.
	if session, ok := s.sessions.Session(req.Target, childSession); ok {
		if state, stateErr := session.State(ctx); stateErr == nil {
			switch state {
			case agent.StateCompleted, agent.StateInputRequired,
				agent.StateFailed, agent.StateCanceled, agent.StateRejected:
				outcome, err := s.outcome(ctx, session, req.Target, childSession, call, state)
				if err != nil {
					s.failJob(childSession, err.Error())
					return Outcome{}, err
				}
				s.finishJob(childSession, outcome)
				return outcome, nil
			case agent.StateWorking:
				s.failJob(childSession, "the child is already running")
				return Outcome{}, errdefs.Conflictf(
					"delegation: child %q is already running", childSession)
			}
		}
	}

	s.wg.Add(1)
	go s.runJob(childSession, call, message.NewTextMessage(message.RoleUser, req.Input))
	return outcomeOf(created), nil
}

func (s *Service) runJob(id string, call Call, msg message.Message) {
	defer s.wg.Done()
	if err := s.store.Update(s.ctx, id, func(job *Job) {
		if !terminal(job.Status) {
			job.Status = StatusRunning
		}
	}); err != nil {
		return
	}
	job, err := s.store.Get(s.ctx, id)
	if err != nil {
		return
	}
	s.execute(job, call, msg)
}

func (s *Service) execute(job Job, call Call, msg message.Message) {
	ctx := withParentTrace(WithCall(s.ctx, call), job.TraceParent)
	if s.policy.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.policy.Timeout)
		defer cancel()
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		s.failJob(job.ID, ctx.Err().Error())
		return
	}

	session, ok := s.sessions.Session(job.Target, job.ChildSessionID)
	if !ok {
		s.failJob(job.ID, "the target is not deployed")
		return
	}

	var text strings.Builder
	for event, err := range session.Run(ctx, msg) {
		if err != nil {
			s.failJob(job.ID, err.Error())
			return
		}
		if typed, ok := event.(agent.Text); ok {
			text.WriteString(typed.Text)
		}
	}
	state, err := session.State(ctx)
	if err != nil {
		s.failJob(job.ID, err.Error())
		return
	}
	outcome, err := s.outcome(ctx, session, job.Target, job.ChildSessionID, call, state)
	if err != nil {
		s.failJob(job.ID, err.Error())
		return
	}
	if len(outcome.Content.Parts) == 0 && text.Len() > 0 {
		outcome.Content = message.NewTextContent(text.String())
	}
	s.finishJob(job.ID, outcome)
}

// Status returns one async job, scoped to the parent session that owns it.
func (s *Service) Status(ctx context.Context, id string) (Outcome, error) {
	job, err := s.jobFor(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	return outcomeOf(job), nil
}

// Answer resumes a child that paused for input.
func (s *Service) Answer(ctx context.Context, id, answer string) (Outcome, error) {
	job, err := s.jobFor(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	if job.Status != StatusInputRequired {
		return Outcome{}, errdefs.Validationf(
			"delegation: job %q is %s, not input-required", id, job.Status)
	}
	if strings.TrimSpace(answer) == "" {
		return Outcome{}, errdefs.Validationf("delegation: answer is required")
	}
	if err := s.store.Update(ctx, id, func(job *Job) {
		job.Status = StatusRunning
		job.Question = nil
		job.Error = ""
	}); err != nil {
		return Outcome{}, err
	}
	call := Call{Depth: job.Depth, ChainID: job.ChainID}
	s.wg.Add(1)
	go s.runAnswer(id, call, answer)
	updated, err := s.store.Get(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	return outcomeOf(updated), nil
}

func (s *Service) runAnswer(id string, call Call, answer string) {
	defer s.wg.Done()
	job, err := s.store.Get(s.ctx, id)
	if err != nil {
		return
	}
	s.execute(job, call, message.NewTextMessage(message.RoleUser, answer))
}

// Cancel stops one async job.
func (s *Service) Cancel(ctx context.Context, id string) (Outcome, error) {
	job, err := s.jobFor(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	if terminal(job.Status) {
		return outcomeOf(job), nil
	}
	if session, ok := s.sessions.Session(job.Target, job.ChildSessionID); ok {
		_ = session.Cancel(ctx, "delegation canceled")
	}
	if err := s.store.Update(ctx, id, func(job *Job) {
		job.Status = StatusCanceled
	}); err != nil {
		return Outcome{}, err
	}
	updated, err := s.store.Get(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	return outcomeOf(updated), nil
}

// Close stops every async worker this service owns.
func (s *Service) Close() error {
	s.close.Do(func() {
		s.cancel()
		s.wg.Wait()
	})
	return nil
}

func (s *Service) jobFor(ctx context.Context, id string) (Job, error) {
	run, ok := runctx.From(ctx)
	if !ok || run.SessionID == "" {
		return Job{}, errdefs.Validationf("delegation: a parent session is required")
	}
	job, err := s.store.Get(ctx, id)
	if errors.Is(err, ErrJobNotFound) {
		return Job{}, errdefs.NotFoundf("delegation: job %q", id)
	}
	if err != nil {
		return Job{}, err
	}
	if job.ParentSessionID != run.SessionID {
		return Job{}, errdefs.NotFoundf("delegation: job %q", id)
	}
	return job, nil
}

func (s *Service) finishJob(id string, outcome Outcome) {
	_ = s.store.Update(s.ctx, id, func(job *Job) {
		if terminal(job.Status) {
			return
		}
		job.Status = outcome.Status
		job.Content = outcome.Content.Clone()
		job.Question = nil
		if outcome.Question != nil {
			question := outcome.Question.Clone()
			job.Question = &question
		}
		job.Error = outcome.Error
	})
}

func (s *Service) failJob(id, reason string) {
	_ = s.store.Update(s.ctx, id, func(job *Job) {
		if terminal(job.Status) {
			return
		}
		job.Status = StatusFailed
		job.Error = reason
	})
}

func outcomeOf(job Job) Outcome {
	return Outcome{
		Status:       job.Status,
		ID:           job.ID,
		Target:       job.Target,
		ChildSession: job.ChildSessionID,
		Content:      job.Content.Clone(),
		Question:     job.Question,
		Error:        job.Error,
		Depth:        job.Depth,
		ChainID:      job.ChainID,
	}
}

func terminal(status Status) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusCanceled:
		return true
	}
	return false
}
