package manager

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft"
)

// InstanceID identifies one managed instance inside a Group. The
// meaning is application-defined (a tenant, a profile, a workspace).
type InstanceID string

var instanceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidateInstanceID checks the id spelling.
func ValidateInstanceID(id InstanceID) error {
	if !instanceIDPattern.MatchString(string(id)) {
		return errdefs.Validationf(
			"craft manager: invalid instance id %q", id)
	}
	return nil
}

// Spec describes one managed instance.
type Spec struct {
	ID InstanceID
	// Options configure the instance. An empty Paths field derives
	// <GroupOptions.Root>/<id>/{config,data}.
	Options Options
}

// GroupOptions configures a Group.
type GroupOptions struct {
	// Root is the instance directory root.
	Root string
	// MaxInstances bounds the number of live instances; <= 0 means 32.
	MaxInstances int
	// StartTimeout bounds each instance build/start.
	StartTimeout time.Duration
}

// GroupEvent describes one instance state transition.
type GroupEvent struct {
	InstanceID InstanceID
	State      State
	Err        string
}

// Group hosts several isolated Managers in one process. Each instance
// keeps its own paths, lock, Craft and state machine; sharing only
// happens through the Spec's Build closure.
type Group struct {
	opts GroupOptions

	mu        sync.Mutex
	instances map[InstanceID]*Manager
	subs      map[uint64]func(GroupEvent)
	nextSub   uint64
	closed    bool
}

// NewGroup creates the instances without starting them.
func NewGroup(opts GroupOptions, specs ...Spec) (*Group, error) {
	if opts.MaxInstances <= 0 {
		opts.MaxInstances = 32
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = time.Minute
	}
	group := &Group{
		opts:      opts,
		instances: make(map[InstanceID]*Manager),
		subs:      make(map[uint64]func(GroupEvent)),
	}
	for _, spec := range specs {
		if _, err := group.add(spec); err != nil {
			return nil, err
		}
	}
	return group, nil
}

// Subscribe observes instance state transitions.
func (g *Group) Subscribe(fn func(GroupEvent)) func() {
	if fn == nil {
		return func() {}
	}
	g.mu.Lock()
	g.nextSub++
	id := g.nextSub
	g.subs[id] = fn
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		delete(g.subs, id)
		g.mu.Unlock()
	}
}

func (g *Group) notify(event GroupEvent) {
	g.mu.Lock()
	subs := make([]func(GroupEvent), 0, len(g.subs))
	for _, fn := range g.subs {
		subs = append(subs, fn)
	}
	g.mu.Unlock()
	for _, fn := range subs {
		fn(event)
	}
}

func (g *Group) add(spec Spec) (*Manager, error) {
	if err := ValidateInstanceID(spec.ID); err != nil {
		return nil, err
	}
	if spec.ID == "" {
		return nil, errdefs.Validationf("craft manager: instance id is required")
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, errdefs.NotAvailablef("craft manager group: closed")
	}
	if _, duplicate := g.instances[spec.ID]; duplicate {
		g.mu.Unlock()
		return nil, errdefs.Conflictf(
			"craft manager group: instance %q already exists", spec.ID)
	}
	if len(g.instances) >= g.opts.MaxInstances {
		g.mu.Unlock()
		return nil, errdefs.RateLimitf(
			"craft manager group: max instances reached (%d)",
			g.opts.MaxInstances)
	}
	if spec.Options.Paths.DataDir == "" || spec.Options.Paths.ConfigDir == "" {
		spec.Options.Paths = derivePaths(g.opts.Root, spec.ID, spec.Options.Paths)
	}
	instance, err := New(spec.Options)
	if err != nil {
		g.mu.Unlock()
		return nil, fmt.Errorf("craft manager group: instance %s: %w", spec.ID, err)
	}
	g.instances[spec.ID] = instance
	g.mu.Unlock()
	instance.Subscribe(func(state State) {
		g.notify(GroupEvent{InstanceID: spec.ID, State: state})
		if current := instance.Craft(); current != nil {
			_ = current.Emit(context.Background(), craft.SubjectGroupInstance,
				craft.GroupInstanceEvent{
					InstanceID: string(spec.ID),
					State:      string(state),
				})
		}
	})
	return instance, nil
}

// Add creates and starts one instance.
func (g *Group) Add(ctx context.Context, spec Spec) error {
	instance, err := g.add(spec)
	if err != nil {
		return err
	}
	return instance.Start(ctx)
}

// Remove stops and drops one instance. Data on disk is preserved.
func (g *Group) Remove(ctx context.Context, id InstanceID) error {
	g.mu.Lock()
	instance := g.instances[id]
	delete(g.instances, id)
	g.mu.Unlock()
	if instance == nil {
		return nil
	}
	return instance.Close()
}

// Get returns one instance.
func (g *Group) Get(id InstanceID) (*Manager, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	instance, ok := g.instances[id]
	return instance, ok
}

// Info is the read-side view of one instance.
type Info struct {
	ID    InstanceID
	State State
	Paths Paths
}

// Instances returns the instances in id order.
func (g *Group) Instances() []Info {
	g.mu.Lock()
	ids := make([]InstanceID, 0, len(g.instances))
	for id := range g.instances {
		ids = append(ids, id)
	}
	g.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]Info, 0, len(ids))
	for _, id := range ids {
		if instance, ok := g.Get(id); ok {
			out = append(out, Info{ID: id, State: instance.State()})
		}
	}
	return out
}

// StartAll starts every instance, aggregating failures.
func (g *Group) StartAll(ctx context.Context) error {
	var errs []error
	for _, info := range g.Instances() {
		instance, ok := g.Get(info.ID)
		if !ok {
			continue
		}
		timeoutCtx, cancel := context.WithTimeout(ctx, g.opts.StartTimeout)
		err := instance.Start(timeoutCtx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", info.ID, err))
			g.notify(GroupEvent{
				InstanceID: info.ID, State: StateFailed, Err: err.Error(),
			})
		}
	}
	return errors.Join(errs...)
}

// StopAll stops every instance, aggregating failures.
func (g *Group) StopAll(ctx context.Context) error {
	var errs []error
	for _, info := range g.Instances() {
		instance, ok := g.Get(info.ID)
		if !ok {
			continue
		}
		if err := instance.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", info.ID, err))
		}
	}
	return errors.Join(errs...)
}

// Run starts every instance, runs one runner goroutine per instance,
// and stops everything when ctx ends or a runner fails.
func (g *Group) Run(ctx context.Context, runner Runner) error {
	if runner == nil {
		return errdefs.Validationf("craft manager group: runner is required")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := g.StartAll(runCtx); err != nil {
		return err
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, info := range g.Instances() {
		instance, ok := g.Get(info.ID)
		if !ok {
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := runner.Run(runCtx, instance.Craft()); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("instance %s: %w", info.ID, err))
				mu.Unlock()
				cancel()
			}
		}()
	}
	wait.Wait()
	stopErr := g.StopAll(context.WithoutCancel(ctx))
	mu.Lock()
	defer mu.Unlock()
	return errors.Join(append(errs, stopErr)...)
}

// Close stops every instance and marks the group closed.
func (g *Group) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return g.StopAll(ctx)
}

// derivePaths fills missing roots from the group root.
func derivePaths(root string, id InstanceID, paths Paths) Paths {
	if strings.TrimSpace(root) == "" {
		return paths
	}
	base := filepath.Join(root, string(id))
	if paths.ConfigDir == "" {
		paths.ConfigDir = filepath.Join(base, "config")
	}
	if paths.DataDir == "" {
		paths.DataDir = filepath.Join(base, "data")
	}
	if paths.AppHome == "" {
		paths.AppHome = paths.DataDir
	}
	return paths
}
