package delegation

import (
	"context"
	"iter"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// Mode is how long the caller waits for the delegated work.
type Mode string

const (
	// ModeSync waits for the child's terminal result.
	ModeSync Mode = "sync"
	// ModeAsync admits the work and returns; the first slice rejects it.
	ModeAsync Mode = "async"
)

// Validate reports whether the mode is defined.
func (m Mode) Validate() error {
	switch m {
	case ModeSync, ModeAsync:
		return nil
	default:
		return errdefs.Validationf("delegation: unknown mode %q", m)
	}
}

// Status is the caller-visible lifecycle of one delegation.
type Status string

const (
	// StatusRunning means the child is still working.
	StatusRunning Status = "running"
	// StatusCompleted means the child produced a final answer.
	StatusCompleted Status = "completed"
	// StatusInputRequired means the child asked the caller a question.
	StatusInputRequired Status = "input_required"
	// StatusFailed means the child failed or was rejected.
	StatusFailed Status = "failed"
	// StatusCanceled means the child was stopped.
	StatusCanceled Status = "canceled"
)

// Policy is what the calling agent is allowed to delegate and how much.
type Policy struct {
	// Targets is an allowlist; empty means every deployed agent except self.
	Targets []string
	// Modes is an allowlist; empty means sync only.
	Modes []Mode
	// MaxDepth bounds nested delegations. Zero means four.
	MaxDepth int
	// MaxConcurrent bounds in-flight sync delegations. Zero means four.
	MaxConcurrent int
	// Timeout bounds one delegation. Zero means the caller's context only.
	Timeout time.Duration
	// MaxInputBytes bounds the input text. Zero means 64 KiB.
	MaxInputBytes int
}

// Session is the child handle a host exposes. It mirrors deploy/host Session
// without importing either package.
type Session interface {
	Run(ctx context.Context, msg message.Message, opts ...agent.TurnOption) iter.Seq2[agent.Event, error]
	State(ctx context.Context) (agent.TaskState, error)
	Cancel(ctx context.Context, reason string) error
	Result(ctx context.Context) (agent.Result, error)
}

// Sessions resolves deployed agents by name.
type Sessions interface {
	Session(name, sessionID string) (Session, bool)
	Names() []string
}

// Request is one delegation.
type Request struct {
	Mode   Mode
	Target string
	Input  string
}

// Outcome is what the caller gets back.
type Outcome struct {
	Status       Status
	ID           string
	Target       string
	ChildSession string
	Content      message.Content
	Question     *message.Content
	Error        string
	Depth        int
	ChainID      string
}

// Call is the delegation lineage a child run inherits. It is a value so a
// later field (a budget, a root session id, an eval marker) can be added
// without inventing another context key.
type Call struct {
	// Depth is the number of delegation edges above this run. The root caller
	// sees zero.
	Depth int
	// ChainID identifies the root delegation of this chain. Every descendant
	// keeps it; async jobs persist it so a resumed child stays in the chain.
	ChainID string
}

// Child returns the call a delegated run inherits.
func (c Call) Child() Call {
	c.Depth++
	return c
}

type callKey struct{}

// WithCall stamps the delegation lineage on a context.
func WithCall(ctx context.Context, call Call) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callKey{}, call)
}

// CallFrom returns the delegation lineage; absent means the root caller.
func CallFrom(ctx context.Context) (Call, bool) {
	if ctx == nil {
		return Call{}, false
	}
	call, ok := ctx.Value(callKey{}).(Call)
	return call, ok
}

// DepthFromContext is the common read for tools and trace attributes.
func DepthFromContext(ctx context.Context) int {
	call, _ := CallFrom(ctx)
	return call.Depth
}

func (s *Service) allowedTarget(target, caller string, names []string) bool {
	if !contains(names, target) {
		return false
	}
	if len(s.policy.Targets) == 0 {
		return target != caller
	}
	return contains(s.policy.Targets, target)
}

func (s *Service) allowedMode(mode Mode) bool {
	if len(s.policy.Modes) == 0 {
		return mode == ModeSync
	}
	for _, allowed := range s.policy.Modes {
		if allowed == mode {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func cleanID(value string) string {
	return strings.NewReplacer("/", "_", "\\", "_").Replace(value)
}
