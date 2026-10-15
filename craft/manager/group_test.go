package manager

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft"
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
