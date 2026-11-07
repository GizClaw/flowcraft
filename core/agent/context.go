package agent

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// ContextPolicy decides what the model sees for one round.
//
// The loop has exactly one such decision — "send everything the Task has, minus
// the input" — and this interface is where it stops being hard-coded. A policy
// that summarises old messages, keeps a sliding window, or injects retrieved
// context is an application's choice; none of those words appear in this
// package, and the default policy keeps the previous behaviour exactly.
//
// A policy must be a pure function of its input: the same Projection has to
// produce the same Projected. Durable state travels in State, not in fields,
// because one policy serves every conversation of an agent and a restarted
// process must be able to reproduce the projection it made before.
type ContextPolicy interface {
	Project(ctx context.Context, in Projection) (Projected, error)
}

// Projection is everything a policy may look at for one round.
type Projection struct {
	// System is the Task-level prompt.
	System message.Content
	// History is the complete history the Task has, minus Next.
	History []message.Message
	// Next is the message this round sends.
	Next inference.GenerateInput
	// Model and Limits name what will receive the request. Limits is the zero
	// value when the deployment declared no descriptor.
	Model  model.ModelRef
	Limits model.ModelLimits
	// Tools are the tools this round offers, so a policy can weigh their schemas.
	Tools []message.ToolDefinition
	// State is what this policy returned last time, restored from the
	// checkpoint. Nil on the first round of a Task.
	State json.RawMessage
}

// Projected is the policy's answer.
type Projected struct {
	// Context replaces the messages sent as the request's context.
	Context []message.Message
	// State is persisted with the next checkpoint and handed back on restore.
	// Nil means "nothing to persist".
	State json.RawMessage
}

// FullHistory is the default policy: every message the Task has, minus the input.
type FullHistory struct{}

var _ ContextPolicy = FullHistory{}

func (FullHistory) Project(_ context.Context, in Projection) (Projected, error) {
	return Projected{Context: in.History, State: in.State}, nil
}

// sectionContextPolicy is the checkpoint section a policy's state lives in.
//
// A section rather than a new column or event: the checkpoint already carries
// opaque, owner-keyed state for the adapters above it, and a policy is one more
// owner. The agent never decodes it.
const sectionContextPolicy = "context"

// project applies the policy to one round.
func (s *Session) project(
	ctx context.Context,
	task *Task,
	history []message.Message,
	next inference.GenerateInput,
	tools inference.Toolset,
) ([]message.Message, error) {
	in := Projection{
		System:  task.System,
		History: history,
		Next:    next,
		Model:   s.runtime.DefaultModel(),
		Tools:   tools.Definitions(),
		State:   s.policyState,
	}
	if descriptor, ok := s.describeModel(ctx); ok {
		in.Limits = descriptor.Limits
	}
	out, err := s.policy.Project(ctx, in)
	if err != nil {
		return nil, err
	}
	if out.State != nil {
		s.policyState = out.State
	}
	return out.Context, nil
}

// describeModel returns the descriptor of the default model, so a policy can read
// the input window instead of guessing it.
func (s *Session) describeModel(context.Context) (model.ModelDescriptor, bool) {
	catalog := s.runtime.Provider().Catalog
	if catalog == nil {
		return model.ModelDescriptor{}, false
	}
	ref := s.runtime.DefaultModel()
	if ref.ID.Name == "" {
		return model.ModelDescriptor{}, false
	}
	return catalog.Describe(ref.ID)
}
