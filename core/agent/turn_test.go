package agent_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/identity/inject"
	"github.com/GizClaw/flowcraft/core/outcome"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// sessionCtx is the context a host hands to RunTurn: one with a session attached, which
// is what lets the turn decorator attach its own identity.
func sessionCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := inject.Session(context.Background(), identity.Session{
		Tenant:    "acme",
		Agent:     "helper",
		SessionID: "s1",
		Bind:      "bind-1",
	})
	if err != nil {
		t.Fatalf("inject.Session: %v", err)
	}
	return ctx
}

func request(store agent.Store) agent.Request {
	return agent.Request{
		Agent:       "helper",
		Task:        agent.Task{ID: "t1", SessionID: "s1", State: agent.StateWorking},
		Input:       agenttest.UserMessage("hi"),
		Binding:     tooltest.Binding(),
		Ordinal:     1,
		Fingerprint: "fp-1",
		Store:       store,
	}
}

// collect drains a turn's stream.
func collect(seq func(func(agent.Event, error) bool)) ([]agent.Event, error) {
	var events []agent.Event
	var failure error
	seq(func(event agent.Event, err error) bool {
		if err != nil {
			failure = err
			return false
		}
		events = append(events, event)
		return true
	})
	return events, failure
}

// TestRunTurnForwardsAndSavesTerminalState is the decorator's contract: every event
// reaches the caller, and where the task stopped reaches the store.
func TestRunTurnForwardsAndSavesTerminalState(t *testing.T) {
	store := agenttest.NewMemStore()
	driver := agenttest.NewScripted(
		agent.StreamStart{Round: 1, Model: "gpt"},
		agent.Text{Text: "he"},
		agent.Text{Text: "llo"},
		agenttest.Terminal(agent.StateCompleted),
	).OpenDriver()
	events, err := collect(agent.RunTurn(sessionCtx(t), request(store), driver))
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}

	saved, err := store.Load(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Task.State != agent.StateCompleted {
		t.Fatalf("saved state = %q, want completed", saved.Task.State)
	}
}

// TestRunTurnSumsUsage keeps the accounting in one place: a hook and a record both read
// the turn's totals, and neither should have to fold the stream itself.
func TestRunTurnSumsUsage(t *testing.T) {
	driver := agenttest.NewScripted(
		agent.UsageEvent{Usage: usage(10, 5)},
		agent.UsageEvent{Usage: usage(2, 3)},
		agenttest.Terminal(agent.StateCompleted),
	).OpenDriver()
	var outcome agent.TurnOutcome
	hooks := agent.Hooks{AfterTurn: func(_ context.Context, _ agent.TurnInfo, got agent.TurnOutcome) error {
		outcome = got
		return nil
	}}
	req := request(nil)
	req.Hooks = hooks
	if _, err := collect(agent.RunTurn(sessionCtx(t), req, driver)); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if outcome.Usage.InputTokens != 12 || outcome.Usage.OutputTokens != 8 {
		t.Fatalf("usage = %d/%d, want 12/8", outcome.Usage.InputTokens, outcome.Usage.OutputTokens)
	}
	if outcome.State != agent.StateCompleted {
		t.Fatalf("state = %q", outcome.State)
	}
}

// TestRunTurnClassifiesADriverFailure keeps the diagnosis reachable: the error goes to
// the caller and the classification is readable from the context the turn ran on.
func TestRunTurnClassifiesADriverFailure(t *testing.T) {
	driver := agenttest.NewScripted(agent.Text{Text: "partial"}).FailAt(context.DeadlineExceeded).OpenDriver()
	var kind agent.FailureKind
	hooks := agent.Hooks{AfterTurn: func(ctx context.Context, _ agent.TurnInfo, outcome agent.TurnOutcome) error {
		kind, _ = agent.FailureKindFrom(ctx)
		if outcome.Err == nil {
			t.Error("the outcome carries no error")
		}
		return nil
	}}
	req := request(nil)
	req.Hooks = hooks
	_, err := collect(agent.RunTurn(sessionCtx(t), req, driver))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the driver's", err)
	}
	if kind != agent.FailureTimeout {
		t.Fatalf("kind = %q, want timeout", kind)
	}
}

// TestRunTurnStopsWhenTheConsumerDoes: a streaming caller that has seen enough ends the
// turn, and the turn reports it as a cancellation rather than as a driver failure.
func TestRunTurnStopsWhenTheConsumerDoes(t *testing.T) {
	driver := agenttest.NewScripted(
		agent.Text{Text: "one"},
		agent.Text{Text: "two"},
		agenttest.Terminal(agent.StateCompleted),
	).OpenDriver()
	var seen []string
	agent.RunTurn(sessionCtx(t), request(nil), driver)(func(event agent.Event, err error) bool {
		if err != nil {
			return false
		}
		if text, ok := event.(agent.Text); ok {
			seen = append(seen, text.Text)
		}
		return false // stop after the first event
	})
	if len(seen) != 1 {
		t.Fatalf("seen = %v, want one event", seen)
	}
}

// TestRunTurnDrivesTheSteerPath is the whole steering contract end to end: queue an
// input, have the driver apply it at a boundary, and observe it in order with the rest of
// the stream.
func TestRunTurnDrivesTheSteerPath(t *testing.T) {
	program := agenttest.NewScripted(
		agent.Text{Text: "before"},
		agent.Text{Text: "after"},
		agenttest.Terminal(agent.StateCompleted),
	)
	program.SteerAt = 1
	driver := program.OpenDriver()

	// The driver is this session's instance, so the input needs no key.
	if _, err := driver.Steer(agent.SteerInput{Message: agenttest.UserMessage("stop"), Intent: agent.SteerRedirect}); err != nil {
		t.Fatalf("Steer: %v", err)
	}

	events, err := collect(agent.RunTurn(sessionCtx(t), request(nil), driver))
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	sawInterrupted, boundary := false, 0
	for _, event := range events {
		if interruption, ok := event.(agent.Interrupted); ok {
			sawInterrupted = true
			boundary = interruption.Boundary
		}
	}
	if !sawInterrupted {
		t.Fatal("no Interrupted event: the applied input was invisible")
	}
	if boundary != 1 {
		t.Fatalf("boundary = %d, want 1", boundary)
	}
	if applied := driver.Applied(); len(applied) != 1 || applied[0].Intent != agent.SteerRedirect {
		t.Fatalf("applied = %+v", applied)
	}
}

// TestRunTurnWithoutADriverIsAnErrorNotAPanic: a host wiring bug is reported, and the
// process survives it.
func TestRunTurnWithoutADriver(t *testing.T) {
	_, err := collect(agent.RunTurn(sessionCtx(t), request(nil), nil))
	if err == nil {
		t.Fatal("RunTurn accepted a nil driver")
	}
}

// TestRunTurnWithoutAStoreKeepsNothing is the stateless deployment: the turn runs, and no
// store is required to have one.
func TestRunTurnWithoutAStoreKeepsNothing(t *testing.T) {
	driver := agenttest.NewScripted(agent.Text{Text: "x"}, agenttest.Terminal(agent.StateCompleted)).OpenDriver()
	if _, err := collect(agent.RunTurn(sessionCtx(t), request(nil), driver)); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
}

func usage(input, output int64) outcome.Usage {
	return outcome.Usage{InputTokens: input, OutputTokens: output}
}

// probeDriver runs fn as the driver's whole turn, so a test can observe what the
// decorator did to the context.
type probeDriver struct {
	fn     func(ctx context.Context, yield func(agent.Event, error) bool)
	closed bool
}

func (p *probeDriver) Run(ctx context.Context, _ agent.Request) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) { p.fn(ctx, yield) }
}

func (p *probeDriver) Close() error { p.closed = true; return nil }

// TestEarlyStopCancelsTheDriver is P1: when the consumer stops, the driver's context is
// cancelled before its iterator resumes, so a goroutine-backed driver cannot outlive the
// turn.
func TestEarlyStopCancelsTheDriver(t *testing.T) {
	var observed error
	driver := &probeDriver{fn: func(ctx context.Context, yield func(agent.Event, error) bool) {
		if !yield(agent.Text{Text: "one"}, nil) {
			// Resumed after the decorator cancelled: the driver must see it.
			observed = ctx.Err()
		}
	}}

	agent.RunTurn(sessionCtx(t), request(nil), driver)(func(agent.Event, error) bool { return false })
	if !errors.Is(observed, context.Canceled) {
		t.Fatalf("driver context = %v, want cancelled", observed)
	}
}

// TestDriverPanicBecomesATerminalFailure is P5: a panicking driver must not escape as a
// panic (the caller is often a fan-out goroutine), and the turn must still leave a
// terminal state behind.
func TestDriverPanicBecomesATerminalFailure(t *testing.T) {
	store := agenttest.NewMemStore()
	driver := &probeDriver{fn: func(_ context.Context, _ func(agent.Event, error) bool) {
		panic("boom")
	}}

	_, err := collect(agent.RunTurn(sessionCtx(t), request(store), driver))
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v, want a panic report", err)
	}
	saved, loadErr := store.Load(context.Background(), "t1")
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if saved.Task.State != agent.StateFailed {
		t.Fatalf("saved state = %q, want failed", saved.Task.State)
	}
}

// TestTerminalCheckpointCarriesTheFingerprint is P3: a resume must be able to refuse a
// different deployment, which requires the checkpoint to record which one wrote it.
func TestTerminalCheckpointCarriesTheFingerprint(t *testing.T) {
	store := agenttest.NewMemStore()
	driver := agenttest.NewScripted(agenttest.Completed()).OpenDriver()
	if _, err := collect(agent.RunTurn(sessionCtx(t), request(store), driver)); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	saved, err := store.Load(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Fingerprint != "fp-1" {
		t.Fatalf("fingerprint = %q, want fp-1", saved.Fingerprint)
	}
}

// TestEventLogKeepsTheFactsNotTheStream is P2: the terminal state, an applied operator
// input and an artifact are persisted; stream deltas are not, because the log is evidence
// and the stream is the product.
func TestEventLogKeepsTheFactsNotTheStream(t *testing.T) {
	store := agenttest.NewMemStore()
	driver := agenttest.NewScripted(
		agent.Text{Text: "streamed"},
		agent.Interrupted{Interruption: agent.Interruption{Intent: agent.SteerRedirect, Boundary: 2}},
		agent.ArtifactWritten{Artifact: agent.Artifact{ID: "a1", Blob: "sha256:x"}},
		agenttest.Completed(),
	).OpenDriver()
	if _, err := collect(agent.RunTurn(sessionCtx(t), request(store), driver)); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	events, err := store.Events(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var kinds []string
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	joined := strings.Join(kinds, ",")
	if strings.Contains(joined, "text") {
		t.Fatalf("the stream was persisted: %s", joined)
	}
	for _, want := range []string{"state", "interrupted", "artifact"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("kinds = %s, missing %q", joined, want)
		}
	}
}

// TestEventKindsAreMapped pins the sealed interface's other half: every event type names
// its own kind, and no two share one. A swap here would be invisible to a consumer's type
// switch and ruinous to a wire vocabulary.
func TestEventKindsAreMapped(t *testing.T) {
	for want, event := range map[agent.EventKind]agent.Event{
		agent.KindStreamStart: agent.StreamStart{},
		agent.KindText:        agent.Text{},
		agent.KindReasoning:   agent.Reasoning{},
		agent.KindToolCall:    agent.ToolCallStart{},
		agent.KindToolResult:  agent.ToolResult{},
		agent.KindArtifact:    agent.ArtifactWritten{},
		agent.KindUsage:       agent.UsageEvent{},
		agent.KindStatus:      agent.Status{},
		agent.KindInterrupted: agent.Interrupted{},
	} {
		if got := event.Kind(); got != want {
			t.Errorf("%T.Kind() = %q, want %q", event, got, want)
		}
	}
}

// TestHooksChainStopsAtTheFirstFailure pins the ordering rule: a later hook must not act
// on a session or a message its predecessor rejected.
func TestHooksChainStopsAtTheFirstFailure(t *testing.T) {
	var ran []string
	hooks := agent.Chain(
		agent.Hooks{BeforeTurn: func(context.Context, *agent.TurnInfo) error {
			ran = append(ran, "first")
			return errors.New("refused")
		}},
		agent.Hooks{BeforeTurn: func(context.Context, *agent.TurnInfo) error {
			ran = append(ran, "second")
			return nil
		}},
	)
	turn := agent.TurnInfo{}
	if err := hooks.BeforeTurn(context.Background(), &turn); err == nil {
		t.Fatal("the chain swallowed a refusal")
	}
	if len(ran) != 1 || ran[0] != "first" {
		t.Fatalf("ran = %v, want only the first hook", ran)
	}
}

// TestHooksAfterTurnRunsEveryStage is the asymmetry: one policy's bookkeeping is not
// another's to cancel, so an after-turn failure is logged and the chain continues.
func TestHooksAfterTurnRunsEveryStage(t *testing.T) {
	var ran int
	hooks := agent.Chain(
		agent.Hooks{AfterTurn: func(context.Context, agent.TurnInfo, agent.TurnOutcome) error {
			ran++
			return errors.New("bookkeeping failed")
		}},
		agent.Hooks{AfterTurn: func(context.Context, agent.TurnInfo, agent.TurnOutcome) error {
			ran++
			return nil
		}},
	)
	if err := hooks.AfterTurn(context.Background(), agent.TurnInfo{}, agent.TurnOutcome{}); err != nil {
		t.Fatalf("AfterTurn = %v, want nil (failures are logged)", err)
	}
	if ran != 2 {
		t.Fatalf("ran %d hooks, want 2", ran)
	}
}

// TestHooksEmpty is the fast path: a deployment with no hooks must not pay for the
// plumbing, so "nothing to run" has to be reportable.
func TestHooksEmpty(t *testing.T) {
	if !(agent.Hooks{}).Empty() {
		t.Fatal("zero Hooks reported itself non-empty")
	}
	if !agent.Chain().Empty() {
		t.Fatal("a chain of nothing is not empty")
	}
	if !agent.Chain(agent.Hooks{}).Empty() {
		t.Fatal("a chain of empty hooks is not empty")
	}
	busy := agent.Hooks{AfterTurn: func(context.Context, agent.TurnInfo, agent.TurnOutcome) error { return nil }}
	if busy.Empty() {
		t.Fatal("a hook was reported as nothing to run")
	}
}

func TestSteerIntentValid(t *testing.T) {
	for _, intent := range []agent.SteerIntent{agent.SteerRedirect, agent.SteerCorrect} {
		if !intent.Valid() {
			t.Fatalf("%q reported invalid", intent)
		}
	}
	if agent.SteerIntent("cancel").Valid() {
		t.Fatal("an unknown intent reported valid")
	}
}

func TestTruncateThroughOption(t *testing.T) {
	settings := agent.SaveOptions{}
	agent.TruncateThrough(7, "state")(&settings)
	if !settings.Truncate || settings.Through != 7 || len(settings.Kinds) != 1 || settings.Kinds[0] != "state" {
		t.Fatalf("settings = %+v", settings)
	}
}

// TestRunTurnWithoutAnOrdinalStillRuns keeps a host bug from taking the turn down: the
// span loses its turn identity, and the caller still gets the stream.
func TestRunTurnWithoutAnOrdinalStillRuns(t *testing.T) {
	req := request(nil)
	req.Ordinal = 0
	driver := agenttest.NewScripted(agent.Text{Text: "x"}, agenttest.Completed()).OpenDriver()
	events, err := collect(agent.RunTurn(sessionCtx(t), req, driver))
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d", len(events))
	}
}

// TestRunTurnDeliversADriverErrorOnce is the regression for a double yield: driveTurn reported the
// error and RunTurn reported it again, which a `for range` consumer panics on ("range function
// continued iteration after function for loop body returned false") while a direct call silently
// tolerated.
func TestRunTurnDeliversADriverErrorOnce(t *testing.T) {
	driver := agenttest.NewScripted(agent.Text{Text: "partial"}).FailAt(context.DeadlineExceeded).OpenDriver()

	delivered := 0
	var seen error
	for _, err := range agent.RunTurn(sessionCtx(t), request(nil), driver) {
		if err != nil {
			delivered++
			seen = err
		}
	}
	if delivered != 1 {
		t.Fatalf("the error was delivered %d times, want once", delivered)
	}
	if !errors.Is(seen, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the driver's", seen)
	}
}
