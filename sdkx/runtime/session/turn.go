package session

import (
	"context"
	"errors"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// Turn is one asynchronous execution owned by a Session.
type Turn struct {
	session *Session
	runID   string
	runCtx  context.Context
	cancel  context.CancelFunc

	interrupts chan agent.Interrupt
	done       chan struct{}

	mu          sync.Mutex
	state       TurnState
	result      *agent.Result
	err         error
	interrupt   *agent.Interrupt
	host        agent.Host
	prompts     map[string]*promptEntry
	attachments []*queuedSink
}

func newTurn(session *Session, runID string, parent context.Context) *Turn {
	runCtx, cancel := context.WithCancel(parent)
	return &Turn{
		session:    session,
		runID:      runID,
		runCtx:     runCtx,
		cancel:     cancel,
		interrupts: make(chan agent.Interrupt, 1),
		done:       make(chan struct{}),
		state:      TurnStarting,
		prompts:    make(map[string]*promptEntry),
	}
}

// RunID returns the immutable root execution identifier.
func (t *Turn) RunID() string {
	if t == nil {
		return ""
	}
	return t.runID
}

// State returns the current lifecycle state.
func (t *Turn) State() TurnState {
	if t == nil {
		return TurnFailed
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// Interrupt cooperatively asks the engine to stop. The first cause wins.
func (t *Turn) Interrupt(interrupt agent.Interrupt) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if t.state.isTerminal() || t.interrupt != nil {
		t.mu.Unlock()
		return nil
	}
	saved := interrupt
	t.interrupt = &saved
	t.state = TurnInterrupting
	select {
	case t.interrupts <- interrupt:
	default:
	}
	prompts := t.interruptPendingPromptsLocked(interrupt)
	t.mu.Unlock()
	t.finishPromptActivity(prompts)
	return nil
}

// Wait waits for terminal completion without affecting execution.
func (t *Turn) Wait(ctx context.Context) (*agent.Result, error) {
	if t == nil {
		return nil, errdefs.Validationf("runtime session: nil Turn")
	}
	if ctx == nil {
		return nil, errdefs.Validationf("runtime session: Wait context is required")
	}
	select {
	case <-t.done:
		return t.terminalResult()
	default:
	}
	select {
	case <-t.done:
		return t.terminalResult()
	case <-ctx.Done():
		return nil, errdefs.FromContext(ctx.Err())
	}
}

func (t *Turn) terminalResult() (*agent.Result, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.result, t.err
}

func (t *Turn) execute(instance *deploy.Instance, request agent.Request) {
	result, err := instance.Execute(t.runCtx, request, agent.WithHost(t.host))
	t.finish(result, err)
}

func (t *Turn) finish(result *agent.Result, err error) {
	defer t.cancel()

	t.mu.Lock()
	if t.state.isTerminal() {
		t.mu.Unlock()
		return
	}
	t.result = result
	t.err = err
	t.state = terminalState(result, err)
	prompts := t.closePendingPromptsLocked()
	attachments := append([]*queuedSink(nil), t.attachments...)
	t.mu.Unlock()

	t.finishPromptActivity(prompts)
	var runEndErr *agent.RunEndPublishError
	runEndFailed := result != nil && errors.As(result.Err, &runEndErr)
	for _, attachment := range attachments {
		if result == nil || runEndFailed {
			detachErr := err
			if runEndFailed {
				detachErr = runEndErr
			}
			attachment.detach(detachErr)
		} else {
			attachment.wait()
		}
	}
	t.session.turnFinished(t)

	close(t.done)
}

func terminalState(result *agent.Result, err error) TurnState {
	if result != nil {
		switch result.Status {
		case agent.StatusCompleted:
			return TurnCompleted
		case agent.StatusInterrupted:
			return TurnInterrupted
		case agent.StatusCanceled:
			return TurnCanceled
		case agent.StatusAborted:
			return TurnAborted
		default:
			return TurnFailed
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return TurnCanceled
		}
	}
	return TurnFailed
}

func (t *Turn) shutdown() {
	_ = t.Interrupt(agent.Interrupt{Cause: agent.CauseHostShutdown})
}
