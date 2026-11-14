package delegation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Service delegates work to deployed agents resolved through Sessions.
type Service struct {
	sessions Sessions
	policy   Policy
	slots    chan struct{}
	store    Store
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	close    sync.Once
}

// Option adjusts a service.
type Option func(*Service)

// WithStore replaces the default in-memory job store.
func WithStore(store Store) Option {
	return func(s *Service) { s.store = store }
}

// New builds a service. The policy defaults are applied here, so every caller
// sees the effective limits.
func New(sessions Sessions, policy Policy, opts ...Option) (*Service, error) {
	if sessions == nil {
		return nil, errdefs.Validationf("delegation: sessions is nil")
	}
	if len(policy.Modes) > 0 {
		for _, mode := range policy.Modes {
			if err := mode.Validate(); err != nil {
				return nil, err
			}
		}
	}
	switch {
	case policy.MaxDepth == 0:
		policy.MaxDepth = 4
	case policy.MaxDepth < 0:
		return nil, errdefs.Validationf("delegation: max_depth must not be negative")
	}
	switch {
	case policy.MaxConcurrent == 0:
		policy.MaxConcurrent = 4
	case policy.MaxConcurrent < 0:
		return nil, errdefs.Validationf("delegation: max_concurrent must not be negative")
	}
	switch {
	case policy.MaxInputBytes == 0:
		policy.MaxInputBytes = 64 << 10
	case policy.MaxInputBytes < 0:
		return nil, errdefs.Validationf("delegation: max_input_bytes must not be negative")
	}
	if policy.Timeout < 0 {
		return nil, errdefs.Validationf("delegation: timeout must not be negative")
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		sessions: sessions,
		policy:   policy,
		slots:    make(chan struct{}, policy.MaxConcurrent),
		store:    NewMemStore(),
		ctx:      ctx,
		cancel:   cancel,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(service)
		}
	}
	if service.store == nil {
		service.store = NewMemStore()
	}
	return service, nil
}

// Policy returns the effective policy.
func (s *Service) Policy() Policy { return s.policy }

// Targets lists the deployed agents this caller may address.
func (s *Service) Targets(ctx context.Context) ([]string, error) {
	run, ok := runctx.From(ctx)
	if !ok || run.AgentName == "" {
		return nil, errdefs.Validationf("delegation: a parent agent is required")
	}
	names := s.sessions.Names()
	out := make([]string, 0, len(names))
	for _, name := range names {
		if s.allowedTarget(name, run.AgentName, names) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Delegate runs one synchronous delegation.
//
// The child session id is derived from the parent session, target and tool
// call id, so a retried tool call addresses the same child. A child that is
// already terminal or waiting for input is read back instead of run again.
func (s *Service) Delegate(ctx context.Context, req Request) (Outcome, error) {
	if req.Mode == "" {
		req.Mode = ModeSync
	}
	if err := req.Mode.Validate(); err != nil {
		return Outcome{}, err
	}
	if !s.allowedMode(req.Mode) {
		return Outcome{}, errdefs.Validationf("delegation: mode %q is not allowed", req.Mode)
	}
	if strings.TrimSpace(req.Target) == "" {
		return Outcome{}, errdefs.Validationf("delegation: target is required")
	}
	if strings.TrimSpace(req.Input) == "" {
		return Outcome{}, errdefs.Validationf("delegation: input is required")
	}
	if len(req.Input) > s.policy.MaxInputBytes {
		return Outcome{}, errdefs.Validationf(
			"delegation: input is %d bytes, limit is %d", len(req.Input), s.policy.MaxInputBytes)
	}

	run, ok := runctx.From(ctx)
	if !ok || run.SessionID == "" {
		return Outcome{}, errdefs.Validationf("delegation: a parent session is required")
	}
	callID, ok := tool.CallIDFromContext(ctx)
	if !ok {
		return Outcome{}, errdefs.Validationf("delegation: a tool call id is required")
	}
	if !s.allowedTarget(req.Target, run.AgentName, s.sessions.Names()) {
		return Outcome{}, errdefs.Validationf("delegation: target %q is not allowed", req.Target)
	}
	call, ok := CallFrom(ctx)
	if !ok {
		call = Call{}
	}
	if call.ChainID == "" {
		call.ChainID = chainID(run, callID)
	}
	if call.Depth >= s.policy.MaxDepth {
		return Outcome{}, errdefs.Validationf(
			"delegation: depth %d reaches the limit %d", call.Depth, s.policy.MaxDepth)
	}
	childCall := call.Child()

	childSession := cleanID(run.SessionID) + "/d/" + cleanID(req.Target) + "/" + cleanID(callID)

	if req.Mode == ModeAsync {
		return s.startAsync(ctx, req, run, callID, childCall, childSession)
	}

	session, ok := s.sessions.Session(req.Target, childSession)
	if !ok {
		return Outcome{}, errdefs.NotFoundf("delegation: target %q is not deployed", req.Target)
	}

	// A retried tool call must not run a terminal child again.
	if state, err := session.State(ctx); err == nil {
		switch state {
		case agent.StateCompleted, agent.StateInputRequired,
			agent.StateFailed, agent.StateCanceled, agent.StateRejected:
			return s.outcome(ctx, session, req.Target, childSession, childCall, state)
		case agent.StateWorking:
			return Outcome{}, errdefs.Conflictf("delegation: child %q is already running", childSession)
		}
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return Outcome{}, ctx.Err()
	}

	runCtx := WithCall(ctx, childCall)
	if s.policy.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, s.policy.Timeout)
		defer cancel()
	}

	var text strings.Builder
	messageIn := message.NewTextMessage(message.RoleUser, req.Input)
	for event, runErr := range session.Run(runCtx, messageIn) {
		if runErr != nil {
			return Outcome{}, runErr
		}
		switch typed := event.(type) {
		case agent.Text:
			text.WriteString(typed.Text)
		}
	}
	state, err := session.State(ctx)
	if err != nil {
		return Outcome{}, err
	}
	outcome, err := s.outcome(ctx, session, req.Target, childSession, childCall, state)
	if err != nil {
		return Outcome{}, err
	}
	if len(outcome.Content.Parts) == 0 && text.Len() > 0 {
		outcome.Content = message.NewTextContent(text.String())
	}
	return outcome, nil
}

func chainID(run runctx.Run, callID string) string {
	sum := sha256.Sum256([]byte(run.SessionID + "\x00" + run.TaskID + "\x00" + callID))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Service) outcome(
	ctx context.Context, session Session, target, childSession string,
	call Call, state agent.TaskState,
) (Outcome, error) {
	out := Outcome{
		ID:           childSession,
		Target:       target,
		ChildSession: childSession,
		Depth:        call.Depth,
		ChainID:      call.ChainID,
	}
	result, err := session.Result(ctx)
	if err != nil && !errors.Is(err, agent.ErrResultUnsupported) {
		return Outcome{}, err
	}
	switch state {
	case agent.StateCompleted:
		out.Status = StatusCompleted
		out.Content = result.Content
	case agent.StateInputRequired:
		out.Status = StatusInputRequired
		out.Question = result.Question
	case agent.StateCanceled:
		out.Status = StatusCanceled
	case agent.StateFailed, agent.StateRejected, agent.StateAuthRequired:
		out.Status = StatusFailed
		out.Error = string(state)
	default:
		out.Status = StatusRunning
	}
	return out, nil
}
