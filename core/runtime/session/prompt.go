package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
)

type promptState uint8

const (
	promptPending promptState = iota
	promptReplied
	promptExpired
	promptInterrupted
	promptClosed
)

type promptOutcome struct {
	reply agent.UserReply
	err   error
}

type promptEntry struct {
	state   promptState
	outcome chan promptOutcome
}

// promptResolution records one prompt that left the pending state, so
// the caller can publish PromptResolved after releasing the turn lock.
type promptResolution struct {
	promptID string
	status   PromptStatus
}

// promptResolvePublishTimeout bounds the best-effort PromptResolved
// publish so a slow subscriber can never stall turn finalization or
// interrupt handling.
const promptResolvePublishTimeout = 2 * time.Second

// Reply supplies a response to one prompt without affecting other prompts.
func (t *Turn) Reply(ctx context.Context, promptID string, reply agent.UserReply) error {
	if t == nil {
		return ErrPromptClosed
	}
	if ctx == nil {
		return errdefs.Validationf("runtime session: Reply context is required")
	}
	if err := ctx.Err(); err != nil {
		return errdefs.FromContext(err)
	}

	t.mu.Lock()
	entry := t.prompts[promptID]
	if entry == nil {
		t.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrPromptUnknown, promptID)
	}
	switch entry.state {
	case promptReplied:
		t.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrPromptDuplicate, promptID)
	case promptPending:
		entry.state = promptReplied
		entry.outcome <- promptOutcome{reply: reply}
		t.mu.Unlock()
		t.session.changeActivity(activityPrompt, -1)
		t.publishPromptResolved(t.host, promptID, PromptReplied)
		return nil
	default:
		t.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrPromptClosed, promptID)
	}
}

func (t *Turn) askUser(ctx context.Context, prompt agent.UserPrompt) (agent.UserReply, error) {
	if ctx == nil {
		return agent.UserReply{}, errdefs.Validationf("runtime session: AskUser context is required")
	}
	promptID, err := randomID()
	if err != nil {
		return agent.UserReply{}, err
	}
	entry := &promptEntry{state: promptPending, outcome: make(chan promptOutcome, 1)}
	t.session.changeActivity(activityPrompt, 1)

	t.mu.Lock()
	if t.state.isTerminal() {
		t.mu.Unlock()
		t.session.changeActivity(activityPrompt, -1)
		return agent.UserReply{}, ErrPromptClosed
	}
	if t.interrupt != nil {
		interrupt := *t.interrupt
		t.mu.Unlock()
		t.session.changeActivity(activityPrompt, -1)
		return agent.UserReply{}, agent.Interrupted(interrupt)
	}
	t.prompts[promptID] = entry
	host := t.host
	t.mu.Unlock()

	requested := PromptRequested{
		RunID:    t.runID,
		TurnID:   t.runID,
		PromptID: promptID,
		Prompt:   prompt,
	}
	envelope, err := event.NewEnvelope(ctx, SubjectPromptRequested(t.runID), requested)
	if err == nil {
		envelope.SetRunID(t.runID)
		envelope.SetAgentID(t.session.key.AgentID)
		err = host.Publish(ctx, envelope)
	}
	if err != nil {
		if t.resolvePrompt(promptID, promptClosed, promptOutcome{err: err}) {
			t.session.changeActivity(activityPrompt, -1)
			t.publishPromptResolved(host, promptID, PromptClosed)
		}
		return agent.UserReply{}, err
	}

	select {
	case outcome := <-entry.outcome:
		return outcome.reply, outcome.err
	case <-ctx.Done():
		ctxErr := errdefs.FromContext(ctx.Err())
		if t.resolvePrompt(promptID, promptExpired, promptOutcome{err: ctxErr}) {
			t.session.changeActivity(activityPrompt, -1)
			t.publishPromptResolved(host, promptID, PromptExpired)
			return agent.UserReply{}, ctxErr
		}
		outcome := <-entry.outcome
		return outcome.reply, outcome.err
	case <-t.done:
		outcome := <-entry.outcome
		return outcome.reply, outcome.err
	}
}

func (t *Turn) resolvePrompt(promptID string, state promptState, outcome promptOutcome) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.prompts[promptID]
	if entry == nil || entry.state != promptPending {
		return false
	}
	entry.state = state
	entry.outcome <- outcome
	return true
}

func (t *Turn) interruptPendingPromptsLocked(interrupt agent.Interrupt) []promptResolution {
	var resolved []promptResolution
	for promptID, entry := range t.prompts {
		if entry.state != promptPending {
			continue
		}
		entry.state = promptInterrupted
		entry.outcome <- promptOutcome{err: agent.Interrupted(interrupt)}
		resolved = append(resolved, promptResolution{
			promptID: promptID,
			status:   PromptInterrupted,
		})
	}
	return resolved
}

func (t *Turn) closePendingPromptsLocked() []promptResolution {
	var resolved []promptResolution
	for promptID, entry := range t.prompts {
		if entry.state != promptPending {
			continue
		}
		entry.state = promptClosed
		entry.outcome <- promptOutcome{err: ErrPromptClosed}
		resolved = append(resolved, promptResolution{
			promptID: promptID,
			status:   PromptClosed,
		})
	}
	return resolved
}

func (t *Turn) finishPromptActivity(count int) {
	if count > 0 {
		t.session.changeActivity(activityPrompt, -count)
	}
}

// publishPromptResolved emits one best-effort PromptResolved envelope.
// It is called only after the turn lock is released — Publish can block
// on Block-backpressure subscribers, so it must never run under t.mu.
func (t *Turn) publishPromptResolved(host agent.Host, promptID string, status PromptStatus) {
	if t == nil || host == nil || promptID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(t.runCtx),
		promptResolvePublishTimeout,
	)
	defer cancel()
	envelope, err := event.NewEnvelope(ctx, SubjectPromptResolved(t.runID), PromptResolved{
		RunID:    t.runID,
		TurnID:   t.runID,
		PromptID: promptID,
		Status:   status,
	})
	if err != nil {
		return
	}
	envelope.SetRunID(t.runID)
	if t.session != nil {
		envelope.SetAgentID(t.session.key.AgentID)
	}
	_ = host.Publish(ctx, envelope)
}

func randomID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", errdefs.Internal(fmt.Errorf("runtime session: allocate id: %w", err))
	}
	return hex.EncodeToString(bytes[:]), nil
}
