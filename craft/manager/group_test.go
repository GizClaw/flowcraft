package manager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/hooks"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/craft"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

func writeInstanceDefinition(t *testing.T, root, id string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "craft.yaml")
	if err := os.WriteFile(path, []byte(definition), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestGroupLifecycle(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	specs := []Spec{
		{ID: "alpha", Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "alpha"),
			Capabilities:   []craft.Capability{testCapability{}},
		}},
		{ID: "beta", Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "beta"),
			Capabilities:   []craft.Capability{testCapability{}},
		}},
	}
	group, err := NewGroup(GroupOptions{Root: root}, specs...)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	ctx := context.Background()
	if err := group.StartAll(ctx); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	infos := group.Instances()
	if len(infos) != 2 || infos[0].ID != "alpha" || infos[1].ID != "beta" {
		t.Fatalf("instances = %+v", infos)
	}
	for _, info := range infos {
		if info.State != StateRunning {
			t.Fatalf("instance %s state = %s", info.ID, info.State)
		}
	}
	if err := group.StopAll(ctx); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if err := group.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type countingRunner struct {
	mu    sync.Mutex
	count int
}

func (r *countingRunner) Run(context.Context, *craft.Craft) error {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
	// Return immediately: Run then stops every instance.
	return nil
}

// TestGroupRunReclaimsWhatItStarted pins what Run leaves behind when one
// instance cannot start. Run returning means the group is not running,
// so the instances that did start have to be stopped: nothing else
// reaches them, and each one holds its Craft, its goroutines and its
// single-instance lock. The caller's context cannot be what stops them,
// because it may be the very thing that made the failing instance fail.
func TestGroupRunReclaimsWhatItStarted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataDir := t.TempDir()
	group, err := NewGroup(GroupOptions{Root: root},
		Spec{ID: "alpha", Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "alpha"),
			Paths:          Paths{DataDir: dataDir},
			Lock:           true,
			Capabilities:   []craft.Capability{testCapability{}},
		}},
		// The definition is not there, so this instance cannot start —
		// after alpha is already up, which is the half that matters.
		Spec{ID: "broken", Options: Options{
			DefinitionPath: filepath.Join(root, "broken", "craft.yaml"),
			Capabilities:   []craft.Capability{testCapability{}},
		}},
	)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	runner := &countingRunner{}
	if err := group.Run(context.Background(), runner); err == nil {
		t.Fatal("Run reported success for a group whose instance cannot start")
	}
	// Run never got as far as the runner: the start failed first.
	if runner.count != 0 {
		t.Fatalf("runner ran %d times after a failed start, want 0", runner.count)
	}
	for _, info := range group.Instances() {
		if info.State == StateRunning || info.State == StateStarting {
			t.Fatalf("instance %s was left in state %s by Run", info.ID, info.State)
		}
	}
	// The reclaim is complete, not just visible in the states: the
	// single-instance lock alpha took is free again, which a second
	// manager over the same root and data directory proves by taking it.
	rebuild, err := NewGroup(GroupOptions{Root: root},
		Spec{ID: "alpha", Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "alpha"),
			Paths:          Paths{DataDir: dataDir},
			Lock:           true,
			Capabilities:   []craft.Capability{testCapability{}},
		}},
	)
	if err != nil {
		t.Fatalf("NewGroup after the reclaim: %v", err)
	}
	if err := rebuild.StartAll(context.Background()); err != nil {
		t.Fatalf("start after the reclaim: %v", err)
	}
	if err := rebuild.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if err := rebuild.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestGroupRunAndMaxInstances(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	spec := func(id string) Spec {
		return Spec{ID: InstanceID(id), Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, id),
			Capabilities:   []craft.Capability{testCapability{}},
		}}
	}
	group, err := NewGroup(GroupOptions{Root: root, MaxInstances: 1}, spec("alpha"))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	runner := &countingRunner{}
	if err := group.Run(context.Background(), runner); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.count != 1 {
		t.Fatalf("runner count = %d, want 1", runner.count)
	}
	if _, err := NewGroup(GroupOptions{Root: root, MaxInstances: 1},
		spec("alpha"), spec("beta")); err == nil {
		t.Fatal("MaxInstances was not enforced")
	}
}

// TestGroupRemoveKeepsWhatItCouldNotStop pins what a group does with an
// instance whose close did not happen: it keeps it. Dropping the
// instance first would take the only handle to a Craft that is still
// running — and still holds its single-instance lock — leaving it, its
// goroutines and its plugin processes unreachable, which is the state
// the group exists to prevent. The refused instance stays where the
// caller can reach it, and the next Remove — with the transition in
// flight over — stops it and drops it.
func TestGroupRemoveKeepsWhatItCouldNotStop(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gate := newBuildGate()
	group, err := NewGroup(GroupOptions{Root: root},
		Spec{ID: "alpha", Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "alpha"),
			Lock:           true,
			Build:          gate.build,
		}},
	)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	startErr := make(chan error, 1)
	go func() { startErr <- group.StartAll(context.Background()) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never reached the build")
	}
	instance, ok := group.Get("alpha")
	if !ok {
		t.Fatal("the instance is not in the group")
	}
	instance.transitionWait = 30 * time.Millisecond

	// The close the remove runs cannot take the turn while the start
	// holds it, so the remove is refused — and the instance the refusal
	// was about is still the group's.
	if err := group.Remove(context.Background(), "alpha"); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Remove during a start that will not finish = %v, want NotAvailable", err)
	}
	if _, ok := group.Get("alpha"); !ok {
		t.Fatal("the group dropped an instance it could not stop")
	}

	gate.releaseAll()
	if err := <-startErr; err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	// The instance the refusal left behind is the live one, and the next
	// remove does both halves.
	if err := group.Remove(context.Background(), "alpha"); err != nil {
		t.Fatalf("Remove after the refusal: %v", err)
	}
	if _, ok := group.Get("alpha"); ok {
		t.Fatal("the removed instance is still in the group")
	}
	// The stop went with the drop: the single-instance lock is free
	// again, which a second group over the same root proves by taking it.
	rebuilt, err := NewGroup(GroupOptions{Root: root},
		Spec{ID: "alpha", Options: Options{
			DefinitionPath: filepath.Join(root, "alpha", "craft.yaml"),
			Lock:           true,
			Capabilities:   []craft.Capability{testCapability{}},
		}},
	)
	if err != nil {
		t.Fatalf("NewGroup after the remove: %v", err)
	}
	if err := rebuilt.StartAll(context.Background()); err != nil {
		t.Fatalf("a second group could not start over the removed instance: %v", err)
	}
	if err := rebuilt.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
}

// failingHost is a plugin host whose close fails. A stop that ended
// stopped and still reported a problem is what a group keeps as its
// close outcome.
type failingHost struct{}

func (failingHost) Start(context.Context) error { return nil }

func (failingHost) Close() error {
	return errors.New("plugin host: close: hello: its process refused to stop")
}

func (failingHost) Revision() uint64        { return 1 }
func (failingHost) Subscribe(func()) func() { return func() {} }
func (failingHost) Tools() tool.Source      { return plugin.NewToolSet() }
func (failingHost) SkillRoots() []string    { return nil }

func (failingHost) HookSources() []hooks.ExtraSource { return nil }

func (failingHost) Entries() ([]plugin.Entry, error) { return nil, nil }

func (failingHost) CallTool(
	context.Context, string, string, any,
) (json.RawMessage, error) {
	return nil, errors.New("plugin host: no process to call")
}

// TestGroupCloseWaitsForTheCloseInFlight pins what a close reports when
// it arrives while another one is stopping the instances. The flag is
// raised before the stops and lowered again if they did not all happen,
// so a close answered from the flag alone reports one of the two
// outcomes without having waited to see which — "closed" about a group
// whose close is still in flight, or is about to leave it open.
func TestGroupCloseWaitsForTheCloseInFlight(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	group, err := NewGroup(GroupOptions{Root: root},
		Spec{ID: "alpha", Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "alpha"),
			Capabilities:   []craft.Capability{testCapability{}},
		}},
	)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	instance, ok := group.Get("alpha")
	if !ok {
		t.Fatal("the instance is not in the group")
	}
	// State subscribers are notified outside the lock, so a subscriber
	// that waits holds the close inside the stop it is running.
	held := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	instance.Subscribe(func(State) {
		once.Do(func() { close(held) })
		<-release
	})

	firstDone := make(chan error, 1)
	go func() { firstDone <- group.Close() }()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the close never reached the instance")
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- group.Close() }()
	select {
	case err := <-secondDone:
		t.Fatalf("the second close answered %v while the first was still "+
			"stopping the instances", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	first, second := <-firstDone, <-secondDone
	if first != nil || second != nil {
		t.Fatalf("closes = %v and %v, want both nil", first, second)
	}
	if err := group.Add(context.Background(), Spec{ID: "beta", Options: Options{
		DefinitionPath: writeInstanceDefinition(t, root, "beta"),
		Capabilities:   []craft.Capability{testCapability{}},
	}}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Add after the close = %v, want NotAvailable", err)
	}
}

// TestGroupAddBehindACloseTakesTheInstanceBack pins the other direction
// of the same race: the add joins the set, the close's snapshot stops it
// before its start commits, and the start then brings the instance up
// behind a group that says closed. What this call started is what this
// call takes back — an instance running in a closed group is one nothing
// in the group will stop, and an Add reported as done leaves the caller
// with no reason to look for it.
func TestGroupAddBehindACloseTakesTheInstanceBack(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gate := newBuildGate()
	group, err := NewGroup(GroupOptions{Root: root},
		Spec{ID: "alpha", Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "alpha"),
			Capabilities:   []craft.Capability{testCapability{}},
		}},
	)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	// The add is held inside its build: the instance is in the set, and
	// the start has not committed.
	added := Spec{ID: "beta", Options: Options{
		DefinitionPath: writeInstanceDefinition(t, root, "beta"),
		Build:          gate.build,
	}}
	addDone := make(chan error, 1)
	go func() { addDone <- group.Add(context.Background(), added) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the add never reached its build")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- group.Close() }()
	// The close has to be the one that raised the flag before the start
	// commits — that is the interleaving this pins — so the test waits
	// for it to be in flight rather than for a release that would make
	// the order a matter of scheduling.
	deadline := time.Now().Add(5 * time.Second)
	for {
		group.mu.Lock()
		closing := group.closing != nil
		group.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the close never started stopping the instances")
		}
		time.Sleep(time.Millisecond)
	}
	gate.releaseAll()

	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-addDone; !errdefs.IsNotAvailable(err) {
		t.Fatalf("Add behind a close = %v, want NotAvailable", err)
	}
	if _, ok := group.Get("beta"); ok {
		t.Fatal("the closed group kept the instance the add started")
	}
	if _, ok := group.Get("alpha"); !ok {
		t.Fatal("the close dropped the instance it was called with")
	}
}

// TestGroupCloseLeavesNoInstanceToStart pins what "closed" means for the
// instances: the close closes them, so a handle taken before it cannot
// bring one back up in a group that says it is done. A stopped instance
// that a handle can start again is a Craft running with nothing in the
// group that knows about it.
func TestGroupCloseLeavesNoInstanceToStart(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	group, err := NewGroup(GroupOptions{Root: root},
		Spec{ID: "alpha", Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "alpha"),
			Capabilities:   []craft.Capability{testCapability{}},
		}},
	)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	instance, ok := group.Get("alpha")
	if !ok {
		t.Fatal("the instance is not in the group")
	}
	if err := group.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := instance.Start(context.Background()); !errdefs.IsNotAvailable(err) {
		t.Fatalf("starting an instance of a closed group = %v, want NotAvailable", err)
	}
}

// TestGroupCloseKeepsTheOutcomeItClosedWith pins what a close that finds
// the group already closed reports. A stop that ended stopped and
// reported a problem — a Craft whose own close failed — leaves the group
// closed, because its instances are gone: it is final, and the caller
// that closes it again reports the error that close saw rather than a
// fresh success about a failure it never heard about.
func TestGroupCloseKeepsTheOutcomeItClosedWith(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	build := func(
		_ context.Context,
		def craft.Definition,
		_ Paths,
	) (*craft.Craft, error) {
		return craft.New(def, craft.Options{
			Capabilities: []craft.Capability{testCapability{}},
			Plugins:      failingHost{},
		})
	}
	group, err := NewGroup(GroupOptions{Root: root},
		Spec{ID: "alpha", Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "alpha"),
			Build:          build,
		}},
	)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	if err := group.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	instance, ok := group.Get("alpha")
	if !ok {
		t.Fatal("the instance is not in the group")
	}
	first := group.Close()
	if first == nil {
		t.Fatal("the close of a Craft whose close failed reported success")
	}
	if state := instance.State(); state != StateStopped {
		t.Fatalf("state after the close = %s, want stopped", state)
	}
	second := group.Close()
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("the second close = %v, want the first close's %v", second, first)
	}
}

// TestGroupSubscribeSeesTransitions pins the event stream: one
// subscriber is told about each instance's state transitions, and the
// function it returns stops them — a group that kept notifying a
// retired callback keeps whatever that callback holds alive with it.
func TestGroupSubscribeSeesTransitions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	group, err := NewGroup(GroupOptions{Root: root}, Spec{
		ID: "alpha",
		Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "alpha"),
			Capabilities:   []craft.Capability{testCapability{}},
		},
	})
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	var mu sync.Mutex
	var seen []GroupEvent
	unsubscribe := group.Subscribe(func(event GroupEvent) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, event)
	})
	if err := group.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	mu.Lock()
	started := append([]GroupEvent(nil), seen...)
	mu.Unlock()
	if len(started) == 0 {
		t.Fatal("no transition was reported for the started instance")
	}
	last := started[len(started)-1]
	if last.InstanceID != "alpha" || last.State != StateRunning || last.Err != "" {
		t.Fatalf("last transition = %+v, want alpha running without an error", last)
	}

	unsubscribe()
	unsubscribe()
	mu.Lock()
	before := len(seen)
	mu.Unlock()
	if err := group.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	mu.Lock()
	after := len(seen)
	mu.Unlock()
	if after != before {
		t.Fatalf("transitions after unsubscribe = %d, want %d", after, before)
	}
}

// TestGroupCloseKeepsWhatItCouldNotStop is the close half of the same
// contract Remove keeps: a group that could not stop an instance must
// not be marked closed. The flag refuses every later Add, so a group
// marked closed around a live instance — with its Craft, its goroutines
// and its single-instance lock — is a group nothing can repair, and its
// own second Close reports a success that never happened.
func TestGroupCloseKeepsWhatItCouldNotStop(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gate := newBuildGate()
	group, err := NewGroup(GroupOptions{Root: root},
		Spec{ID: "alpha", Options: Options{
			DefinitionPath: writeInstanceDefinition(t, root, "alpha"),
			Lock:           true,
			Build:          gate.build,
		}},
	)
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	startErr := make(chan error, 1)
	go func() { startErr <- group.StartAll(context.Background()) }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never reached the build")
	}
	instance, ok := group.Get("alpha")
	if !ok {
		t.Fatal("the instance is not in the group")
	}
	instance.transitionWait = 30 * time.Millisecond

	// The close cannot take the turn while the start holds it, so it is
	// refused — and refused is the whole answer: the group is still open.
	if err := group.Close(); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Close during a start that will not finish = %v, want NotAvailable", err)
	}
	if _, ok := group.Get("alpha"); !ok {
		t.Fatal("the group dropped an instance it could not stop")
	}
	// The group is not merely still holding the instance: it is open, so
	// an Add is answered rather than refused by a flag no instance can
	// clear.
	if err := group.Add(context.Background(), Spec{ID: "beta", Options: Options{
		DefinitionPath: writeInstanceDefinition(t, root, "beta"),
		Capabilities:   []craft.Capability{testCapability{}},
	}}); err != nil {
		t.Fatalf("Add after the refused close: %v", err)
	}

	gate.releaseAll()
	if err := <-startErr; err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if state := instance.State(); state != StateRunning {
		t.Fatalf("state after the start = %s, want running", state)
	}
	// The next close does both halves the first one refused: it stops the
	// instances and marks the group closed.
	if err := group.Close(); err != nil {
		t.Fatalf("Close after the refusal: %v", err)
	}
	for _, info := range group.Instances() {
		if info.State != StateStopped {
			t.Fatalf("instance %s state after the close = %s, want stopped",
				info.ID, info.State)
		}
	}
	if err := group.Add(context.Background(), Spec{ID: "gamma", Options: Options{
		DefinitionPath: writeInstanceDefinition(t, root, "gamma"),
		Capabilities:   []craft.Capability{testCapability{}},
	}}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Add after the close = %v, want NotAvailable", err)
	}
	// The stops went with the close: the single-instance lock is free
	// again, which a second group over the same root proves by taking it.
	rebuilt, err := NewGroup(GroupOptions{Root: root},
		Spec{ID: "alpha", Options: Options{
			DefinitionPath: filepath.Join(root, "alpha", "craft.yaml"),
			Lock:           true,
			Capabilities:   []craft.Capability{testCapability{}},
		}},
	)
	if err != nil {
		t.Fatalf("NewGroup after the close: %v", err)
	}
	if err := rebuilt.StartAll(context.Background()); err != nil {
		t.Fatalf("a second group could not start over the closed one: %v", err)
	}
	if err := rebuilt.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
}
