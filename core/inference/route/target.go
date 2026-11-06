package route

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// Target is one routable candidate: a provider and the label that identifies it
// in a trace.
//
// It is an inference.Provider because that is exactly what a candidate is — a
// set of seams plus the model line-up that says what it can serve.
type Target struct {
	// Name identifies the target in traces and errors.
	Name string
	// Provider is the candidate.
	Provider inference.Provider
	// Weight is a relative preference for selectors that use one. Zero means one.
	Weight int
	// Model is the model this target serves: vendor plus API model name.
	// Dispatch stamps it onto the request, so a caller names a model and gets
	// the provider identity of the target that actually answered. Zero means the
	// target did not declare one, and the request's own identity is used.
	Model model.ModelID
	// Profile is the deployment identity this target serves. Routing fills it
	// into the request when the caller named none, so the public account follows
	// the target; a caller that sets it pins the deployment.
	Profile string
}

// Selector chooses among targets for one request. One seam, one method: a
// selector that answered for several seams would be the operation table this
// package exists to avoid.
type Selector[Req any] interface {
	Select(ctx context.Context, request Req, targets []Target) ([]Target, error)
}

// Policy configures a Router. A nil selector means "every target, in order"; the
// dispatcher then relies on each driver to reject what it cannot serve before
// any provider I/O.
type Policy struct {
	Generate Selector[inference.GenerateRequest]
	Decision Selector[inference.DecisionRequest]
	Retry    RetryPolicy
}

// Attempt is one target's try, successful or not.
type Attempt struct {
	Target string
	// Kind is the classified failure; empty on success.
	Kind inference.ErrorKind
	// Err is the redacted error text.
	Err string
}

// Trace collects the attempts of one call.
//
// It travels in the context rather than being returned, because the seam
// signatures return only a response: a caller that wants the record creates one
// with WithTrace and reads it afterwards.
type Trace struct {
	mu       sync.Mutex
	attempts []Attempt
}

// Attempts returns a copy of the recorded attempts, oldest first.
func (t *Trace) Attempts() []Attempt {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Attempt(nil), t.attempts...)
}

func (t *Trace) record(attempt Attempt) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.attempts = append(t.attempts, attempt)
	t.mu.Unlock()
}

type traceKey struct{}

// WithTrace attaches a collector to ctx.
func WithTrace(ctx context.Context, trace *Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, trace)
}

// TraceFrom returns the collector attached to ctx, or nil.
func TraceFrom(ctx context.Context) *Trace {
	trace, _ := ctx.Value(traceKey{}).(*Trace)
	return trace
}

// Router dispatches seam calls across targets.
type Router struct {
	targets []Target
	policy  Policy
}

// New validates a target list and its policy.
func New(targets []Target, policy Policy) (*Router, error) {
	if len(targets) == 0 {
		return nil, errors.New("route: at least one target is required")
	}
	seen := make(map[string]bool, len(targets))
	for i, target := range targets {
		if target.Name == "" {
			return nil, fmt.Errorf("route: target %d has no name", i)
		}
		if seen[target.Name] {
			return nil, fmt.Errorf("route: target %q appears twice", target.Name)
		}
		seen[target.Name] = true
		if target.Provider.Generator == nil && target.Provider.Decider == nil {
			return nil, fmt.Errorf("route: target %q serves no seam", target.Name)
		}
	}
	return &Router{targets: targets, policy: policy}, nil
}

// Provider publishes the routed seams as one provider.
//
// A seam with no capable target stays nil, so "this router does not judge" is
// the same kind of fact as "this driver does not embed".
func (r *Router) Provider() inference.Provider {
	provider := inference.Provider{}
	if r.anyTarget(func(t Target) bool { return t.Provider.Generator != nil }) {
		provider.Generator = routedGenerator{router: r}
	}
	if r.anyTarget(func(t Target) bool { return t.Provider.Decider != nil }) {
		provider.Decider = routedDecider{router: r}
	}
	if r.anyTarget(func(t Target) bool { return t.Provider.Catalog != nil }) {
		provider.Catalog = routeCatalog{targets: r.targets}
	}
	return provider
}

func (r *Router) anyTarget(match func(Target) bool) bool {
	for _, target := range r.targets {
		if match(target) {
			return true
		}
	}
	return false
}

// dispatch runs the shared selection loop: select, try, classify, move on, with
// one route span over the whole loop and one event per attempt.
func dispatch[Req, Res any](
	ctx context.Context,
	operation string,
	targets []Target,
	selector Selector[Req],
	request Req,
	modelOf func(Req) string,
	withTarget func(Req, Target) Req,
	invoke func(context.Context, Target, Req) (Res, error),
	retry RetryPolicy,
) (Res, error) {
	ctx, call := startRoute(ctx, operation)
	response, err := dispatchTargets(ctx, call, targets, selector, request, modelOf, withTarget, invoke, retry)
	call.finish(err)
	return response, err
}

// dispatchTargets is the loop itself, kept separate so every return path runs
// through the same span close.
func dispatchTargets[Req, Res any](
	ctx context.Context,
	call *routeCall,
	targets []Target,
	selector Selector[Req],
	request Req,
	modelOf func(Req) string,
	withTarget func(Req, Target) Req,
	invoke func(context.Context, Target, Req) (Res, error),
	retry RetryPolicy,
) (Res, error) {
	var zero Res

	// Filter before selecting: a target that does not serve the requested model
	// would fail as UnknownModel and waste an attempt. Order is preserved, so a
	// nil selector still means "in declaration order".
	candidates := filterTargets(targets, modelOf(request))
	ordered := candidates
	if selector != nil {
		selected, err := selector.Select(ctx, request, candidates)
		if err != nil {
			return zero, err
		}
		ordered = selected
	}
	call.selectTargets(ordered)
	if len(ordered) == 0 {
		return zero, inference.NewError(inference.PolicyDenied, "",
			errors.New("route: no candidate target serves this call"))
	}

	policy := retry.effective(len(ordered))
	trace := TraceFrom(ctx)

	var last error
	tried := 0
	for _, target := range ordered {
		if tried >= policy.MaxTargets {
			break
		}
		tried++
		if tried > 1 {
			call.fallback()
		}

		for attempt := 1; attempt <= policy.AttemptsPerTarget; attempt++ {
			var backoff time.Duration
			if attempt > 1 {
				backoff = policy.backoff(attempt - 1)
				if err := sleep(ctx, backoff); err != nil {
					return zero, err
				}
			}
			response, err := invoke(ctx, target, withTarget(request, target))
			call.attempt(target, attempt, backoff, err)
			if err == nil {
				trace.record(Attempt{Target: target.Name})
				return response, nil
			}
			last = err
			trace.record(Attempt{Target: target.Name, Kind: kindOf(err), Err: err.Error()})

			switch classify(err) {
			case RetryStop:
				return zero, err
			case RetryNextTarget:
				attempt = policy.AttemptsPerTarget // leave the retry loop
			}
		}
	}
	return zero, last
}

// filterTargets keeps the targets whose catalog serves the requested model.
//
// A target with no catalog, or one that declares no models, is kept: absence of
// information is not evidence of absence, and the driver's own UnknownModel is
// still the fallback classification.
func filterTargets(targets []Target, name string) []Target {
	if name == "" {
		return targets
	}
	kept := make([]Target, 0, len(targets))
	for _, target := range targets {
		// A declared model is authoritative: it is what the deployment says this
		// target is for. Without one, fall back to the catalog, and with no
		// catalog either the target stays a candidate and the driver decides.
		if target.Model.Name != "" {
			if target.Model.Name == name {
				kept = append(kept, target)
			}
			continue
		}
		if servesModel(target, name) {
			kept = append(kept, target)
		}
	}
	return kept
}

func servesModel(target Target, name string) bool {
	catalog := target.Provider.Catalog
	if catalog == nil {
		return true
	}
	models := catalog.Models()
	if len(models) == 0 {
		return true
	}
	for _, descriptor := range models {
		if descriptor.ID.Name == name {
			return true
		}
	}
	return false
}

// kindOf extracts the classified kind, if the failure carries one.
func kindOf(err error) inference.ErrorKind {
	var classified *inference.Error
	if errors.As(err, &classified) {
		return classified.Kind
	}
	return ""
}

// routeCatalog merges the model line-ups of the targets that publish one.
type routeCatalog struct {
	targets []Target
}

func (c routeCatalog) Models() []model.ModelDescriptor {
	var out []model.ModelDescriptor
	for _, target := range c.targets {
		if target.Provider.Catalog == nil {
			continue
		}
		out = append(out, target.Provider.Catalog.Models()...)
	}
	return out
}

func (c routeCatalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	for _, target := range c.targets {
		if target.Provider.Catalog == nil {
			continue
		}
		if descriptor, ok := target.Provider.Catalog.Describe(id); ok {
			return descriptor, true
		}
	}
	return model.ModelDescriptor{}, false
}
