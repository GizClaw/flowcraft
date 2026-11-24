package agenttest

import (
	"context"
	"iter"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// Scripted is a program that plays a fixed script: a list of steps, each either an event or
// an error. It is enough to exercise the turn decorator, a hook, and a consumer that stops
// early, without a model or a graph.
//
// It is a [agent.Program]: the script is the compiled program, and [Scripted.Open] hands
// each session its own driver. That is the shape a real program has, and using it here is
// what keeps the fixture honest — a fixture that returned a shared driver would let a test
// pass that no real program could.
type Scripted struct {
	steps []step

	// SteerAt is the step index at which queued input is applied; -1 (the default) never
	// applies it.
	SteerAt int

	mu      sync.Mutex
	drivers int
	closed  bool
}

type step struct {
	event agent.Event
	err   error
}

// NewScripted builds a program that plays events in order.
func NewScripted(events ...agent.Event) *Scripted {
	s := &Scripted{SteerAt: -1}
	for _, event := range events {
		s.steps = append(s.steps, step{event: event})
	}
	return s
}

// FailAt appends an error step. It is how a test makes a turn fail partway.
func (s *Scripted) FailAt(err error) *Scripted {
	s.steps = append(s.steps, step{err: err})
	return s
}

// Open implements agent.Program.
func (s *Scripted) Open(agent.SessionInfo) (agent.Driver, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, agent.ErrRunEnded
	}
	s.drivers++
	return &ScriptedDriver{program: s}, nil
}

// Close implements agent.Program.
func (s *Scripted) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Drivers reports how many drivers this program opened, so a test can assert that Open was
// called once per session rather than once per turn.
func (s *Scripted) Drivers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drivers
}

// OpenDriver is Open with the concrete type, for a test that wants to inspect the driver.
func (s *Scripted) OpenDriver() *ScriptedDriver {
	driver, _ := s.Open(agent.SessionInfo{})
	return driver.(*ScriptedDriver)
}

// ScriptedDriver is one session's driver: the queue and the run counter belong here, not to
// the program, because they are per conversation.
type ScriptedDriver struct {
	program *Scripted

	mu      sync.Mutex
	pending []agent.SteerInput
	applied []agent.Interruption
	nextID  int
	runs    int
	closed  bool
}

// Run implements agent.Driver.
func (d *ScriptedDriver) Run(ctx context.Context, _ agent.Request) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		d.mu.Lock()
		d.runs++
		steerAt := d.program.SteerAt
		steps := d.program.steps
		d.mu.Unlock()

		for i, one := range steps {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			if i == steerAt {
				if !d.drain(ctx, i, yield) {
					return
				}
			}
			if one.err != nil {
				yield(nil, one.err)
				return
			}
			if !yield(one.event, nil) {
				return
			}
		}
	}
}

// drain yields one Interrupted per queued input and records what it applied.
func (d *ScriptedDriver) drain(_ context.Context, boundary int, yield func(agent.Event, error) bool) bool {
	d.mu.Lock()
	pending := d.pending
	d.pending = nil
	d.mu.Unlock()
	for _, input := range pending {
		applied := agent.Interruption{Intent: input.Intent, Boundary: boundary}
		d.mu.Lock()
		d.applied = append(d.applied, applied)
		d.mu.Unlock()
		if !yield(agent.Interrupted{Interruption: applied}, nil) {
			return false
		}
	}
	return true
}

// Steer implements agent.Steerer.
func (d *ScriptedDriver) Steer(input agent.SteerInput) (agent.SteerID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return "", agent.ErrRunEnded
	}
	d.nextID++
	d.pending = append(d.pending, input)
	return agent.SteerID("steer-" + itoa(d.nextID)), nil
}

// Unsteer implements agent.Steerer: it withdraws a queued input, and reports ErrSteerUnknown
// once it has been applied (the fixture keeps no record of applied ids).
func (d *ScriptedDriver) Unsteer(id agent.SteerID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range d.pending {
		if agent.SteerID("steer-"+itoa(d.nextID-len(d.pending)+i+1)) != id {
			continue
		}
		d.pending = append(d.pending[:i], d.pending[i+1:]...)
		return nil
	}
	return agent.ErrSteerUnknown
}

// Applied reports the interruptions this driver yielded.
func (d *ScriptedDriver) Applied() []agent.Interruption {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]agent.Interruption(nil), d.applied...)
}

// Runs reports how many times Run was called on this driver.
func (d *ScriptedDriver) Runs() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runs
}

// Close implements agent.Driver.
func (d *ScriptedDriver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

// Terminal is the status event a program ends a turn with, for building scripts.
func Terminal(state agent.TaskState) agent.Event { return agent.Status{State: state} }

// UserMessage builds the input a turn starts from.
func UserMessage(text string) message.Message {
	return message.NewTextMessage(message.RoleUser, text)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
