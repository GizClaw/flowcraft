package script

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// programCacheLimit bounds the compiled programs one kind keeps. A host normally
// has one script per node, so the limit exists for hot reload: every edit adds a
// version, and the old ones have to fall out.
const programCacheLimit = 64

// Option configures a [Kind].
type Option func(*Kind)

// WithBackends registers the execution engines a document may name. At least one
// is required: a kind that can name no backend cannot run anything.
func WithBackends(backends ...Backend) Option {
	return func(k *Kind) {
		for _, backend := range backends {
			k.add(backend)
		}
	}
}

// WithSource replaces where scripts come from. The default [FileSource] reads
// files relative to the process working directory, which a host should replace
// with a base directory.
func WithSource(source Source) Option {
	return func(k *Kind) {
		if source != nil {
			k.source = source
		}
	}
}

// WithRuntime gives the kind the substrate a tool call needs. A script without an
// allow list never touches it; one that declares a tool and has no runtime fails
// at Build, because a capability the host cannot serve is a configuration error.
func WithRuntime(rt *runtime.Runtime) Option {
	return func(k *Kind) { k.rt = rt }
}

// WithLog routes the lines a script writes. The default discards them: a library
// that logs on its own account cannot be silenced.
func WithLog(w io.Writer) Option {
	return func(k *Kind) {
		if w != nil {
			k.log = w
		}
	}
}

// New builds the node kind.
//
// The error is a wiring mistake a host should hear about at construction: no
// backend, a nil backend, or two backends with one name.
func New(opts ...Option) (graph.NodeKind, error) {
	k := &Kind{
		backends: make(map[string]Backend, 1),
		source:   &FileSource{TTL: 0},
		log:      io.Discard,
		programs: make(map[string]*cachedProgram),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(k)
		}
	}
	if len(k.backends) == 0 {
		return nil, errdefs.Validationf("script: at least one backend is required")
	}
	return k, nil
}

// Kind is the node kind: "script".
type Kind struct {
	backends map[string]Backend
	order    []string
	source   Source
	log      io.Writer
	rt       *runtime.Runtime

	mu       sync.Mutex
	programs map[string]*cachedProgram
	recent   []string
}

type cachedProgram struct {
	program Program
}

// plan is the decoded config with its backend already resolved, so an invocation
// does not repeat the lookup.
type plan struct {
	config  Config
	backend Backend
	// tools is the parsed config.allow: the names this node may dispatch.
	tools map[string]struct{}
}

// Name implements [graph.NodeKind].
func (*Kind) Name() string { return "script" }

// DecodeConfig implements [graph.NodeKind]: it parses the config, resolves the
// backend and refuses a budget the backend cannot honour, all at Build time.
func (k *Kind) DecodeConfig(raw json.RawMessage) (any, error) {
	return k.decode(raw)
}

func (k *Kind) decode(raw json.RawMessage) (plan, error) {
	cfg, err := decodeConfig(raw)
	if err != nil {
		return plan{}, err
	}
	backend, ok := k.backends[cfg.Backend]
	if !ok {
		return plan{}, errdefs.Validationf(
			"script: config.backend %q is not registered; offered: %s",
			cfg.Backend, strings.Join(k.order, ", "))
	}
	if err := checkCapabilities(cfg, backend.Name(), backend.Capabilities()); err != nil {
		return plan{}, err
	}
	tools, err := toolNames(cfg.Allow)
	if err != nil {
		return plan{}, err
	}
	if len(tools) > 0 && k.rt == nil {
		return plan{}, errdefs.Validationf(
			"script: config.allow names %d tool(s) but this kind was built without a runtime",
			len(tools))
	}
	return plan{config: cfg, backend: backend, tools: tools}, nil
}

// Invoke implements [graph.NodeKind].
//
// A script produces state and nothing else, so it never opens a stream: the
// result is static, which is also the cheapest thing the engine can pull.
func (k *Kind) Invoke(ctx context.Context, in graph.Invoke) (graph.NodeStream, error) {
	built, err := graph.ConfigOf(in, k.decode)
	if err != nil {
		return nil, err
	}
	source, err := k.source.Load(ctx, built.config.Path)
	if err != nil {
		return nil, err
	}
	if built.config.Version == VersionPinned {
		if recorded := recordedRun(in, built.config.Path); recorded != "" && recorded != source.Version {
			return nil, errdefs.Validationf(
				"script: node %q: pinned version %s is no longer available; %s is now %s",
				in.Node, recorded, built.config.Path, source.Version)
		}
	}
	program, err := k.program(ctx, built, source)
	if err != nil {
		return nil, err
	}
	state, err := project(in, built.config.Reads)
	if err != nil {
		return nil, err
	}
	host := hostFor(in, k.log, k.rt, built.tools)
	host.start(built.config.Path, source.Version)
	out, err := built.backend.Run(ctx, program, state, built.config.Budget, host)
	if err != nil {
		return nil, nodeError(in.Node, err)
	}
	updates, err := writes(in, built.config, out)
	if err != nil {
		return nil, err
	}
	return graph.Static(graph.Result{State: updates, Records: host.records}), nil
}

// program returns the compiled program for one source version, compiling it once.
//
// The key includes the backend name: two nodes may run the same source through
// different engines, and a program is meaningless to the other one.
func (k *Kind) program(ctx context.Context, built plan, source Script) (Program, error) {
	key := built.backend.Name() + "\x00" + source.Version
	k.mu.Lock()
	if cached, ok := k.programs[key]; ok {
		k.mu.Unlock()
		return cached.program, nil
	}
	k.mu.Unlock()

	compiled, err := built.backend.Compile(ctx, source)
	if err != nil {
		return nil, nodeError("", err)
	}
	if compiled == nil {
		return nil, errdefs.Internalf(
			"script: backend %q compiled %s to nil", built.backend.Name(), source.Name)
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	// A concurrent invocation may have compiled the same version while this one
	// was working; the first result wins and this one is dropped.
	if cached, ok := k.programs[key]; ok {
		return cached.program, nil
	}
	k.programs[key] = &cachedProgram{program: compiled}
	k.recent = append(k.recent, key)
	for len(k.recent) > programCacheLimit {
		oldest := k.recent[0]
		k.recent = k.recent[1:]
		delete(k.programs, oldest)
	}
	return compiled, nil
}

func (k *Kind) add(backend Backend) {
	if backend == nil {
		return
	}
	name := backend.Name()
	if name == "" {
		return
	}
	if _, exists := k.backends[name]; exists {
		return
	}
	k.backends[name] = backend
	k.order = append(k.order, name)
}

// nodeError adds the node name and classifies a failure the backend left
// unclassified. A backend that reports its own category keeps it: the wrap uses
// %w, so the marker stays reachable.
func nodeError(node string, err error) error {
	prefix := "script"
	if node != "" {
		prefix = fmt.Sprintf("script: node %q", node)
	}
	if classified(err) {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return errdefs.Abortedf("%s: %w", prefix, err)
}

// classified reports whether err already carries one of the package's categories.
//
// The list is spelled out rather than inferred because errdefs classifies by
// unexported marker interfaces: there is no "is classified" check to call.
func classified(err error) bool {
	for _, check := range []func(error) bool{
		errdefs.IsNotFound,
		errdefs.IsValidation,
		errdefs.IsUnauthorized,
		errdefs.IsForbidden,
		errdefs.IsConflict,
		errdefs.IsRateLimit,
		errdefs.IsTimeout,
		errdefs.IsInterrupted,
		errdefs.IsAborted,
		errdefs.IsNotAvailable,
		errdefs.IsInternal,
		errdefs.IsBudgetExceeded,
		errdefs.IsPolicyDenied,
	} {
		if check(err) {
			return true
		}
	}
	return false
}

var _ graph.NodeKind = (*Kind)(nil)
