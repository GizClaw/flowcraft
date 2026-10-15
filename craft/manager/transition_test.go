package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft"
)

// buildGate is a Build that holds the manager inside its transition
// until the test releases it, reporting when it was entered.
type buildGate struct {
	entered chan struct{}
	release chan struct{}
	// built collects every instance the gate handed out, in build
	// order, so a test can name the instance one call committed.
	built chan *craft.Craft
}

func newBuildGate() *buildGate {
	return &buildGate{
		entered: make(chan struct{}, 8),
		release: make(chan struct{}),
		built:   make(chan *craft.Craft, 8),
	}
}

// build waits for the gate. The first call blocks; later calls return
// as soon as the test has released the gate once, so the instance a test
// stops is a real one.
func (g *buildGate) build(
	_ context.Context,
	def craft.Definition,
	_ Paths,
) (*craft.Craft, error) {
	g.entered <- struct{}{}
	<-g.release
	instance, err := craft.New(def, craft.Options{
		Capabilities: []craft.Capability{testCapability{}},
	})
	if err != nil {
		return nil, err
	}
	g.built <- instance
	return instance, nil
}

// releaseAll lets every build through.
func (g *buildGate) releaseAll() { close(g.release) }

// TestManagerStopWaitsForAnInFlightStart pins the fence around the
// transitions. A stop that arrives while a start is still building must
// wait for that start to commit and then stop what it committed: a stop
// reported over an instance that is about to appear leaves a Craft
// running with no manager pointing at it — and with the single-instance
// lock released, so the next launch would take a lock whose Craft is
// still alive.
func TestManagerStopWaitsForAnInFlightStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := newBuildGate()
	manager := newTestManager(t, writeDefinition(t, dir), dir, func(opts *Options) {
		opts.Build = gate.build
	})

	startErr := make(chan error, 1)
	go func() { startErr <- manager.Start(context.Background()) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never reached the build")
	}

	stopErr := make(chan error, 1)
	go func() { stopErr <- manager.Stop(context.Background()) }()
	select {
	case err := <-stopErr:
		t.Fatalf("Stop returned %v while the start was still building", err)
	case <-time.After(150 * time.Millisecond):
	}

	gate.releaseAll()
	if err := <-startErr; err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := <-stopErr; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if state := manager.State(); state != StateStopped {
		t.Fatalf("state after both = %s, want stopped", state)
	}
	if current := manager.Craft(); current != nil {
		t.Fatal("the stopped manager still points at an instance")
	}
	// The proof that nothing was left running: the single-instance lock
	// is free again, so a second manager over the same data directory can
	// take it and start its own instance.
	second := newTestManager(t, writeDefinition(t, dir), dir, nil)
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("a second manager could not start: %v", err)
	}
	if err := second.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestManagerReplaceIsNotOvertakenByStop covers the replace half of the
// same fence with the interleaving that used to lose an instance: the
// replacement is building, a stop arrives, and the stop's drain must not
// reach the instance the replace is about to retire — nor may the
// replace commit onto a manager that has stopped.
func TestManagerReplaceIsNotOvertakenByStop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := newBuildGate()
	manager := newTestManager(t, writeDefinition(t, dir), dir, func(opts *Options) {
		opts.Build = gate.build
	})
	// The first build is the Start's; let it through at once.
	go func() {
		<-gate.entered
		gate.release <- struct{}{}
	}()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	original := manager.Craft()

	def, err := craft.ParseDefinition([]byte(definition))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	replaceErr := make(chan error, 1)
	go func() { replaceErr <- manager.Replace(context.Background(), def) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the replace never reached the build")
	}

	stopErr := make(chan error, 1)
	go func() { stopErr <- manager.Stop(context.Background()) }()
	select {
	case err := <-stopErr:
		t.Fatalf("Stop returned %v during a replace", err)
	case <-time.After(150 * time.Millisecond):
	}

	gate.releaseAll()
	if err := <-replaceErr; err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if err := <-stopErr; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if state := manager.State(); state != StateStopped {
		t.Fatalf("state after both = %s, want stopped", state)
	}
	if current := manager.Craft(); current != nil {
		t.Fatal("the manager still points at the replacement it stopped")
	}
	// The instance the replace retired is closed, and the one it built
	// was stopped by the stop that waited for it — both locks are gone.
	if err := original.Drain(context.Background()); err != nil {
		t.Fatalf("the retired instance is not usable: %v", err)
	}
	second := newTestManager(t, writeDefinition(t, dir), dir, nil)
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("a second manager could not start: %v", err)
	}
	if err := second.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestManagerReplaceCommitsWhileADrainIsInFlight covers the phase a
// Replace has to tolerate. Drain is a wait, not a transition: it takes
// no turn on the gate, on purpose, so a Close never queues behind a
// drain that may run as long as its caller allows — and for the length
// of that wait it publishes the manager as draining. A Replace that read
// the manager as running and finds it draining at commit has therefore
// not been overtaken: the instance it read is still the current one and
// the instance it built is a running one. Refusing the commit would close
// the instance this call built and report a move that never happened.
func TestManagerReplaceCommitsWhileADrainIsInFlight(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := newBuildGate()
	manager := newTestManager(t, writeDefinition(t, dir), dir, func(opts *Options) {
		opts.Build = gate.build
	})
	// The first build is the Start's; let it through at once.
	go func() {
		<-gate.entered
		gate.release <- struct{}{}
	}()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	original := manager.Craft()

	def, err := craft.ParseDefinition([]byte(definition))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	replaceErr := make(chan error, 1)
	go func() { replaceErr <- manager.Replace(context.Background(), def) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the replace never reached the build")
	}

	// The drain in flight, as it writes itself: publishing the phase is
	// the whole of what a drain does before it waits, and it does it
	// without a turn on the gate. The phase is written here rather than
	// driven through [Manager.Drain] — that wait ends when the Craft's
	// in-flight work does, and this test has no work to hold — so what
	// it pins is the commit: the swap is adopted and the state ends
	// running, which is what a drain's own restore reads before it
	// decides to leave the phase alone.
	manager.mu.Lock()
	manager.state = StateDraining
	manager.mu.Unlock()

	gate.releaseAll()
	if err := <-replaceErr; err != nil {
		t.Fatalf("Replace under a drain in flight: %v", err)
	}
	if state := manager.State(); state != StateRunning {
		t.Fatalf("state after the replace = %s, want running", state)
	}
	replacement := manager.Craft()
	if replacement == nil || replacement == original {
		t.Fatal("the replace did not commit the instance it built")
	}
	// The retired instance is closed, not merely dropped: a Craft left
	// behind keeps its goroutines and endpoints alive with nothing left
	// to reach it.
	if err := original.Start(context.Background()); !errors.Is(err, craft.ErrCraftClosed) {
		t.Fatalf("the retired Craft answers Start with %v, want ErrCraftClosed", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestManagerTransitionWaitIsBounded pins what a transition does when
// the one before it will not finish: it is refused with a reason, and
// the manager is not wedged — the transition that was holding the turn
// still commits, and the next stop works.
func TestManagerTransitionWaitIsBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := newBuildGate()
	manager := newTestManager(t, writeDefinition(t, dir), dir, func(opts *Options) {
		opts.Build = gate.build
	})
	manager.transitionWait = 30 * time.Millisecond

	startErr := make(chan error, 1)
	go func() { startErr <- manager.Start(context.Background()) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never reached the build")
	}
	err := manager.Stop(context.Background())
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("Stop during a start that will not finish = %v, want NotAvailable", err)
	}
	gate.releaseAll()
	if err := <-startErr; err != nil {
		t.Fatalf("Start: %v", err)
	}
	if state := manager.State(); state != StateRunning {
		t.Fatalf("state after the refused stop = %s, want running", state)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after the refusal: %v", err)
	}
	if state := manager.State(); state != StateStopped {
		t.Fatalf("state after the stop = %s, want stopped", state)
	}
}

// TestManagerReplaceIsNotOvertakenByAnotherReplace pins what two
// replacements that overlap leave behind. The transition gate queues the
// second behind the first, so the second replaces what the first
// committed and retires it: both calls pick up the instance the manager
// holds rather than one of its own, which is what keeps a single
// instance — and the single-instance lock — under the manager. Without
// that, each replacement adopts its own instance and the loser is left
// running with nothing pointing at it: a Craft holding its goroutines,
// its plugin processes and a lock nobody knows about.
func TestManagerReplaceIsNotOvertakenByAnotherReplace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := newBuildGate()
	manager := newTestManager(t, writeDefinition(t, dir), dir, func(opts *Options) {
		opts.Build = gate.build
	})
	// The first build is the Start's; let it through at once.
	go func() {
		<-gate.entered
		gate.release <- struct{}{}
	}()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	original := manager.Craft()
	if started := <-gate.built; started != original {
		t.Fatal("the gate did not hand out the instance the start committed")
	}

	def, err := craft.ParseDefinition([]byte(definition))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	firstErr := make(chan error, 1)
	go func() { firstErr <- manager.Replace(context.Background(), def) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first replace never reached the build")
	}

	secondErr := make(chan error, 1)
	go func() { secondErr <- manager.Replace(context.Background(), def) }()
	// A replacement that arrives while another is building waits for the
	// turn instead of taking one of its own.
	select {
	case err := <-secondErr:
		t.Fatalf("the second replace returned %v during the first", err)
	case <-time.After(150 * time.Millisecond):
	}

	gate.releaseAll()
	if err := <-firstErr; err != nil {
		t.Fatalf("first Replace: %v", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("second Replace: %v", err)
	}
	if state := manager.State(); state != StateRunning {
		t.Fatalf("state after both replacements = %s, want running", state)
	}
	swapped := <-gate.built
	current := <-gate.built
	if manager.Craft() != current {
		t.Fatal("the manager does not point at the instance the second replace built")
	}
	// Both instances the replacements walked past are closed, and the one
	// the manager holds is live: an instance that was only dropped keeps
	// its goroutines, its plugin processes and its lock file alive.
	stale := []struct {
		what     string
		instance *craft.Craft
	}{
		{what: "the start's instance", instance: original},
		{what: "the first replace's instance", instance: swapped},
	}
	for _, previous := range stale {
		if err := previous.instance.Start(context.Background()); !errors.Is(err, craft.ErrCraftClosed) {
			t.Fatalf("%s answers Start with %v, want ErrCraftClosed", previous.what, err)
		}
	}
	if err := current.Drain(context.Background()); err != nil {
		t.Fatalf("the current instance is not usable: %v", err)
	}
	// The lock travels with the instance the manager holds: a second
	// launch is refused while that instance is current.
	second := newTestManager(t, writeDefinition(t, dir), dir, nil)
	if err := second.Start(context.Background()); !errdefs.IsConflict(err) {
		t.Fatalf("a second manager started over a live instance: %v", err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	third := newTestManager(t, writeDefinition(t, dir), dir, nil)
	if err := third.Start(context.Background()); err != nil {
		t.Fatalf("a second manager could not start after the stop: %v", err)
	}
	if err := third.Stop(context.Background()); err != nil {
		t.Fatalf("third Stop: %v", err)
	}
}

// TestManagerCloseIsRefusedRatherThanRushed pins what a close arriving
// while a transition will not finish does: it is refused, and the
// manager stays open. The flag that makes a manager final is set by the
// stop, in the same turn on the transition gate, so a refusal sets
// nothing — a manager marked closed behind it would report a successful
// close with a Craft still running and its single-instance lock still
// held, short-circuit every later stop on the flag, and leave that
// instance with no reachable handle. The refused close also has to leave
// the transition it could not wait for alone: the start still commits,
// and the close after it does what the first one could not.
func TestManagerCloseIsRefusedRatherThanRushed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := newBuildGate()
	manager := newTestManager(t, writeDefinition(t, dir), dir, func(opts *Options) {
		opts.Build = gate.build
	})
	manager.transitionWait = 30 * time.Millisecond

	startErr := make(chan error, 1)
	go func() { startErr <- manager.Start(context.Background()) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never reached the build")
	}

	// Close takes its turn, gives up on the transition in flight, and
	// must report that rather than a stop that did not happen.
	if err := manager.Close(); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Close during a start that will not finish = %v, want NotAvailable", err)
	}
	if state := manager.State(); state != StateStarting {
		t.Fatalf("state after the refused close = %s, want starting", state)
	}

	gate.releaseAll()
	if err := <-startErr; err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The refusal marked nothing and stopped nothing: the instance the
	// start committed is live, and the manager does not say otherwise.
	if state := manager.State(); state != StateRunning {
		t.Fatalf("state after the start = %s, want running", state)
	}
	if current := manager.Craft(); current == nil {
		t.Fatal("the manager has no instance after the start")
	}

	// A second Close takes the turn and does both halves the first one
	// refused: it stops the instance and marks the manager final.
	if err := manager.Close(); err != nil {
		t.Fatalf("Close after the refusal: %v", err)
	}
	if state := manager.State(); state != StateStopped {
		t.Fatalf("state after the close = %s, want stopped", state)
	}
	if err := manager.Start(context.Background()); !errdefs.IsNotAvailable(err) {
		t.Fatalf("restart of the closed manager = %v, want not available", err)
	}
	if current := manager.Craft(); current != nil {
		t.Fatal("the closed manager still points at an instance")
	}
	// The lock went with the close: a second launch takes it.
	second := newTestManager(t, writeDefinition(t, dir), dir, nil)
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("a second manager could not start after the close: %v", err)
	}
	if err := second.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}
