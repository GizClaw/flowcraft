package enginetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Capabilities lets an engine declare which optional behaviours it
// implements. The contract suite uses this to skip resume-specific
// subtests when the engine has SupportsResume == false (those
// subtests instead assert the engine returns NotAvailable).
type Capabilities struct {
	// SupportsResume is true when the engine implements Run.ResumeFrom
	// (i.e. given a non-nil ResumeFrom whose ExecID matches Run.ID it
	// resumes the run rather than returning errdefs.NotAvailable).
	SupportsResume bool
}

// Factory builds a fresh engine and reports its capabilities. The
// suite calls Factory once per subtest so subtests do not share
// engine state.
//
// Engines that take no construction arguments can wrap a constructor:
//
//	enginetest.RunSuite(t, enginetest.NewFactory(graph.NewEngine))
//
// or implement Factory directly when the construction needs more
// setup.
type Factory func() (engine.Engine, Capabilities)

// NewFactory adapts a parameterless constructor into a [Factory] that
// reports zero capabilities. Use [Factory] directly when you need to
// declare SupportsResume.
func NewFactory(ctor func() engine.Engine) Factory {
	return func() (engine.Engine, Capabilities) {
		return ctor(), Capabilities{}
	}
}

// RunSuite runs every contract test that applies to the engine
// produced by f. Engines should call this from their own *_test.go:
//
//	func TestEngineContract(t *testing.T) {
//	    enginetest.RunSuite(t, func() (engine.Engine, enginetest.Capabilities) {
//	        return graph.NewEngine(), enginetest.Capabilities{SupportsResume: true}
//	    })
//	}
//
// Each subtest constructs a fresh engine, so failures isolate cleanly.
// The whole suite must pass for an implementation to be considered
// engine.Engine-compliant.
func RunSuite(t *testing.T, f Factory) {
	t.Helper()

	t.Run("CleanCompletion", func(t *testing.T) { testCleanCompletion(t, f) })
	t.Run("ContextCancel", func(t *testing.T) { testContextCancel(t, f) })
	t.Run("CooperativeInterrupt", func(t *testing.T) { testCooperativeInterrupt(t, f) })
	t.Run("InterruptZeroValue", func(t *testing.T) { testInterruptZeroValue(t, f) })
	t.Run("AttributesUntouched", func(t *testing.T) { testAttributesUntouched(t, f) })
	t.Run("PublishErrorTolerated", func(t *testing.T) { testPublishErrorTolerated(t, f) })
	t.Run("ResumeForeignExecID", func(t *testing.T) { testResumeForeignExecID(t, f) })
	t.Run("ResumeNotSupported", func(t *testing.T) { testResumeNotSupported(t, f) })
}

// ---------- subtests ----------

// testCleanCompletion verifies that with no interrupts and a fresh
// board the engine reaches normal completion: nil error, non-nil
// returned Board, returned Board pointer equals the input pointer
// (engines mutate in place by contract).
func testCleanCompletion(t *testing.T, f Factory) {
	t.Helper()
	eng, _ := f()

	host := NewMockHost()
	board := engine.NewBoard()
	run := engine.Run{ID: "run-clean"}

	got, err := eng.Execute(context.Background(), run, host, board)
	if err != nil {
		t.Fatalf("clean Execute returned error: %v", err)
	}
	if got == nil {
		t.Fatal("clean Execute returned nil board")
	}
	if got != board {
		t.Errorf("clean Execute returned a different Board pointer; engines must mutate in place")
	}
}

// testContextCancel verifies that a cancelled context surfaces as
// ctx.Err() and the partial board is still returned.
func testContextCancel(t *testing.T, f Factory) {
	t.Helper()
	eng, _ := f()

	host := NewMockHost()
	board := engine.NewBoard()
	run := engine.Run{ID: "run-cancel"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := eng.Execute(ctx, run, host, board)

	// Engines may finish before noticing cancel (trivial engines that
	// do no work succeed immediately); only assert the error shape
	// when an error is present.
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ctx-cancel error is not context.Canceled / DeadlineExceeded: %v", err)
	}
	if got == nil {
		t.Error("ctx-cancel returned nil board; partial board must still be returned")
	}
}

// testCooperativeInterrupt verifies that a host-injected interrupt
// surfaces as errdefs.IsInterrupted with the cause preserved on the
// destructured InterruptedError.
//
// Engines that complete too fast to observe the interrupt are not
// failing this test — only fail when an error IS returned but it is
// the wrong shape.
func testCooperativeInterrupt(t *testing.T, f Factory) {
	t.Helper()
	eng, _ := f()

	host := NewMockHost()
	host.Interrupt(engine.CauseUserInput, "barge-in")

	board := engine.NewBoard()
	run := engine.Run{ID: "run-intr"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := eng.Execute(ctx, run, host, board)
	if got == nil {
		t.Error("interrupt returned nil board; partial board must still be returned")
	}

	if err == nil {
		t.Skip("engine completed without observing interrupt; not all engines have a select boundary")
	}
	if !errdefs.IsInterrupted(err) {
		t.Fatalf("interrupt error must satisfy errdefs.IsInterrupted; got %v", err)
	}

	var ie engine.InterruptedError
	if !errors.As(err, &ie) {
		t.Fatalf("interrupt error must destructure into engine.InterruptedError; got %T", err)
	}
	if ie.Cause != engine.CauseUserInput {
		t.Errorf("Cause not preserved: want %q got %q", engine.CauseUserInput, ie.Cause)
	}
	if ie.Detail != "barge-in" {
		t.Errorf("Detail not preserved: want %q got %q", "barge-in", ie.Detail)
	}
}

// testInterruptZeroValue verifies an engine that observes a zero-value
// Interrupt still produces a properly classified error.
func testInterruptZeroValue(t *testing.T, f Factory) {
	t.Helper()
	eng, _ := f()

	host := NewMockHost()
	host.Interrupt(engine.CauseUnknown, "")

	board := engine.NewBoard()
	run := engine.Run{ID: "run-zero-intr"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := eng.Execute(ctx, run, host, board)
	if err == nil {
		t.Skip("engine completed without observing interrupt")
	}
	if !errdefs.IsInterrupted(err) {
		t.Fatalf("zero-value interrupt must still satisfy errdefs.IsInterrupted; got %v", err)
	}
}

// testAttributesUntouched verifies the engine does not mutate the
// caller-supplied Attributes map.
func testAttributesUntouched(t *testing.T, f Factory) {
	t.Helper()
	eng, _ := f()

	host := NewMockHost()
	board := engine.NewBoard()
	attrs := map[string]string{
		"tenant":      "acme",
		"engine_kind": "test",
	}
	run := engine.Run{ID: "run-attrs", Attributes: attrs}

	if _, err := eng.Execute(context.Background(), run, host, board); err != nil {
		t.Fatalf("clean Execute returned error: %v", err)
	}

	if got := attrs["tenant"]; got != "acme" {
		t.Errorf("Attributes[tenant] mutated: %q", got)
	}
	if got := attrs["engine_kind"]; got != "test" {
		t.Errorf("Attributes[engine_kind] mutated: %q", got)
	}
	if len(attrs) != 2 {
		t.Errorf("Attributes had keys added/removed; len=%d want 2", len(attrs))
	}
}

// testPublishErrorTolerated verifies that a Publisher returning an
// error does not cause the engine to fail the run. Publish errors are
// observability concerns, never control flow per [engine.Publisher].
func testPublishErrorTolerated(t *testing.T, f Factory) {
	t.Helper()
	eng, _ := f()

	host := NewMockHost()
	host.SetPublishError(errors.New("simulated publish failure"))

	board := engine.NewBoard()
	run := engine.Run{ID: "run-pub-err"}

	_, err := eng.Execute(context.Background(), run, host, board)
	if err != nil {
		t.Fatalf("Execute failed because Publish returned error; "+
			"publish errors must not propagate: %v", err)
	}
}

// testResumeForeignExecID verifies that supplying a checkpoint whose
// ExecID differs from Run.ID is rejected as a validation error,
// regardless of whether the engine supports resume — forking is not
// resuming.
func testResumeForeignExecID(t *testing.T, f Factory) {
	t.Helper()
	eng, _ := f()

	host := NewMockHost()
	board := engine.NewBoard()
	run := engine.Run{
		ID: "run-foreign",
		ResumeFrom: &engine.Checkpoint{
			ExecID: "some-other-run",
			Board:  engine.NewBoard().Snapshot(),
		},
	}

	_, err := eng.Execute(context.Background(), run, host, board)
	if err == nil {
		t.Fatal("Execute with foreign ResumeFrom.ExecID must return an error")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("foreign ExecID must return errdefs.IsValidation; got %v", err)
	}
}

// testResumeNotSupported runs two complementary assertions depending
// on the declared capability:
//
//   - SupportsResume == false: a non-nil ResumeFrom must yield
//     errdefs.IsNotAvailable.
//
//   - SupportsResume == true: a non-nil ResumeFrom whose ExecID
//     matches Run.ID must complete cleanly (engine accepts the
//     resume).
func testResumeNotSupported(t *testing.T, f Factory) {
	t.Helper()
	eng, caps := f()

	host := NewMockHost()
	board := engine.NewBoard()
	run := engine.Run{
		ID: "run-resume",
		ResumeFrom: &engine.Checkpoint{
			ExecID: "run-resume",
			Board:  engine.NewBoard().Snapshot(),
		},
	}

	_, err := eng.Execute(context.Background(), run, host, board)

	if !caps.SupportsResume {
		if err == nil {
			t.Fatal("engine declared SupportsResume=false but accepted ResumeFrom")
		}
		if !errdefs.IsNotAvailable(err) {
			t.Fatalf("non-resumable engine must return errdefs.IsNotAvailable; got %v", err)
		}
		return
	}

	if err != nil {
		t.Fatalf("resumable engine returned error on matching ExecID: %v", err)
	}
}
