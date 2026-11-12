package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"
)

// End is the edge target that finishes a branch.
const End = "end"

// Document is one graph, as written in YAML. Parse accepts JSON too, and
// applies the same strict decoding to either form.
type Document struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	Entry   string `json:"entry"`
	// Input names the state key Start seeds with the run's messages. Empty
	// means the graph takes no input; a caller that supplies some is refused.
	Input      string                   `json:"input"`
	Limits     LimitsDocument           `json:"limits"`
	Checkpoint CheckpointDocument       `json:"checkpoint"`
	Stream     StreamDocument           `json:"stream"`
	State      map[string]StateDocument `json:"state"`
	Nodes      []NodeDocument           `json:"nodes"`
	Edges      []EdgeDocument           `json:"edges"`
	Output     *OutputDocument          `json:"output"`
}

// StreamDocument is what a run puts on the live event stream beside the events
// every run emits (text, reasoning, tool calls, usage).
//
// These are for an observer that renders the run as it happens — a UI showing
// which keys a step changed — and they cost a JSON projection per step, which is
// why nothing is emitted unless the document asks.
type StreamDocument struct {
	// State reports each step's state writes as a GraphState event. Default
	// false: a run nobody is watching should not pay for a projection nobody
	// reads.
	State bool `json:"state"`
}

// CheckpointDocument is how a durable run is written.
//
// Every 0 (the default) writes a base only when the deltas would be missing;
// a positive value also writes one every N steps, bounding how long a restart
// has to replay. A base carries the whole state, so the interval is the price
// paid for a short replay.
type CheckpointDocument struct {
	Every int `json:"every"`
}

// LimitsDocument is what a run may consume.
//
// Zero is the default for each: no cap on steps or routed nodes, no wall-clock
// timeout, and the built-in concurrency. That is safe only because a run is also
// bounded by its caller's context — the limits exist so a document can tighten
// what it needs, not so every document has to spell a budget out.
type LimitsDocument struct {
	// MaxSteps caps how many frontiers a run may execute. 0 means no cap.
	MaxSteps int `json:"max_steps"`
	// MaxNodes caps how many node invocations a run may route, skipped ones
	// included: a cycle of skipped nodes still consumes budget.
	MaxNodes int `json:"max_nodes"`
	// Timeout bounds the wall-clock duration of the whole run, including the
	// time it spends waiting for an answer. Empty means the caller's context is
	// the only bound.
	Timeout string `json:"timeout"`
	// Concurrency caps how many nodes of one frontier run at once.
	Concurrency int `json:"concurrency"`
}

// StateDocument declares one state slot.
type StateDocument struct {
	// Type is text, json, messages or bool.
	Type string `json:"type"`
	// Merge is single (default), last or append.
	Merge string `json:"merge"`
	// Carry keeps this key across the tasks of one session: a new task seeds it
	// from the previous task's state instead of the declared zero value. It is the
	// document's way to say "this is the conversation, not the request".
	//
	// It is meant for bounded state - a game, a form, a plan - which carries in
	// microseconds. A key that grows without bound costs its own size on every
	// turn, because the state is re-read from the store: a 200-message transcript
	// measured 667us and 5000 allocations per turn. A long conversation belongs in
	// one task, not in a carried key.
	Carry bool `json:"carry"`
}

// NodeDocument declares one node instance.
type NodeDocument struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"`
	Config json.RawMessage `json:"config"`
	// Skip is a condition: when it holds, the node is not invoked and its
	// outgoing edges still fire. It routes without running.
	Skip string `json:"skip"`
	// Timeout bounds one invocation. Empty means the step's context is the bound.
	Timeout string `json:"timeout"`
	// Retries is how many times a retryable failure is retried. Zero means once.
	Retries int `json:"retries"`
}

// EdgeDocument is one transition: from a node to a node, or to End.
//
// When is a compiled condition over the state and the step counter; an empty
// condition always fires.
type EdgeDocument struct {
	From string `json:"from"`
	To   string `json:"to"`
	When string `json:"when"`
}

// OutputDocument names the state key a finished run reports.
type OutputDocument struct {
	Key string `json:"key"`
}

// Parse decodes a graph document. YAML is the written form and JSON is accepted
// too; both are converted to one strict decoding, so an unknown field is an
// error either way.
func Parse(body []byte) (Document, error) {
	doc, err := decode.Decode[Document](body)
	if err != nil {
		return Document{}, errdefs.Validationf("graph: document: %w", err)
	}
	return doc, nil
}

// DecodeConfig decodes one node's config strictly.
//
// A node's config is JSON by the time it reaches a kind: the document was YAML,
// the engine converted it once, and an unknown field here is a typo that would
// otherwise be a silently empty setting.
func DecodeConfig(raw json.RawMessage, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return errdefs.Validationf("graph: config: %w", err)
	}
	return nil
}

// nodeSpec is one node after validation.
type nodeSpec struct {
	id      string
	kind    NodeKind
	config  json.RawMessage
	decoded any
	skip    *CompiledCondition
	timeout time.Duration
	retries int
}

// edge is one validated transition.
type edge struct {
	to   string
	when *CompiledCondition
}

// Graph is a built graph: validated, with compiled conditions.
type Graph struct {
	name        string
	entry       string
	input       string
	maxSteps    int
	maxNodes    int
	timeout     time.Duration
	concurrency int
	baseEvery   int
	stateEvents bool
	nodes       map[string]nodeSpec
	order       []string
	edges       map[string][]edge
	state       map[string]StateKey
	carried     map[string]struct{}
	output      string
	blobs       BlobStore
}

// Name is the document's name.
func (g *Graph) Name() string { return g.name }

// Entry is the node a run starts at.
func (g *Graph) Entry() string { return g.entry }

// BlobStore is where a run puts media it answers with. agent.Store satisfies it.
type BlobStore interface {
	PutBlob(ctx context.Context, data []byte) (string, error)
}

// BuildOption configures construction.
type BuildOption func(*buildOptions)

type buildOptions struct {
	blobs BlobStore
}

// WithBlobStore lets a graph answer with media.
func WithBlobStore(store BlobStore) BuildOption {
	return func(o *buildOptions) { o.blobs = store }
}

// defaultConcurrency is how many nodes of one frontier run at once when the
// document names no limit.
const defaultConcurrency = 4

// Build validates a document against a registry and returns the runnable graph.
//
// Everything structural is caught here: an unknown kind, an edge to a node that
// does not exist, a missing entry, a node nothing can reach, a condition that
// does not compile. Cycles are allowed: a frontier that never empties is bounded
// by max_steps instead.
func Build(doc Document, registry *Registry, opts ...BuildOption) (*Graph, error) {
	var settings buildOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}
	if registry == nil {
		return nil, errdefs.Validationf("graph: a node kind registry is required")
	}
	if doc.Version != 1 {
		return nil, errdefs.Validationf("graph: version %d is not supported; this build reads 1", doc.Version)
	}
	state, err := buildState(doc.State)
	if err != nil {
		return nil, err
	}
	limits, err := buildLimits(doc.Limits)
	if err != nil {
		return nil, err
	}
	if doc.Checkpoint.Every < 0 {
		return nil, errdefs.Validationf("graph: checkpoint.every must not be negative")
	}
	g := &Graph{
		name:        doc.Name,
		entry:       strings.TrimSpace(doc.Entry),
		maxSteps:    limits.maxSteps,
		maxNodes:    limits.maxNodes,
		timeout:     limits.timeout,
		concurrency: limits.concurrency,
		baseEvery:   doc.Checkpoint.Every,
		stateEvents: doc.Stream.State,
		nodes:       make(map[string]nodeSpec, len(doc.Nodes)),
		order:       make([]string, 0, len(doc.Nodes)),
		edges:       make(map[string][]edge, len(doc.Nodes)),
		state:       state,
		carried:     carriedKeys(state),
		blobs:       settings.blobs,
	}
	for i, node := range doc.Nodes {
		spec, err := buildNode(i, node, registry, g.nodes)
		if err != nil {
			return nil, err
		}
		g.nodes[spec.id] = spec
		g.order = append(g.order, spec.id)
	}
	if len(g.order) == 0 {
		return nil, errdefs.Validationf("graph: the document declares no nodes")
	}
	if g.entry == "" {
		return nil, errdefs.Validationf("graph: entry is required")
	}
	if _, ok := g.nodes[g.entry]; !ok {
		return nil, errdefs.Validationf("graph: entry %q is not a declared node", g.entry)
	}
	if raw := strings.TrimSpace(doc.Input); raw != "" {
		spec, declared := state[raw]
		if !declared {
			return nil, errdefs.Validationf("graph: input %q is not a declared state key", raw)
		}
		if spec.Kind != KindMessages {
			return nil, errdefs.Validationf(
				"graph: input %q is %s, and the run input is messages", raw, spec.Kind)
		}
		g.input = raw
	}
	if err := g.wireEdges(doc.Edges); err != nil {
		return nil, err
	}
	if err := g.checkReachable(); err != nil {
		return nil, err
	}
	if doc.Output != nil {
		if err := g.wireOutput(*doc.Output); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// limits is the validated form of LimitsDocument.
type limits struct {
	maxSteps    int
	maxNodes    int
	timeout     time.Duration
	concurrency int
}

// buildLimits decodes and validates the run limits.
func buildLimits(doc LimitsDocument) (limits, error) {
	out := limits{
		maxSteps:    doc.MaxSteps,
		maxNodes:    doc.MaxNodes,
		concurrency: doc.Concurrency,
	}
	if out.maxSteps < 0 {
		return limits{}, errdefs.Validationf("graph: limits.max_steps must not be negative")
	}
	if out.maxNodes < 0 {
		return limits{}, errdefs.Validationf("graph: limits.max_nodes must not be negative")
	}
	if out.concurrency < 0 {
		return limits{}, errdefs.Validationf("graph: limits.concurrency must not be negative")
	}
	if out.concurrency == 0 {
		out.concurrency = defaultConcurrency
	}
	if raw := strings.TrimSpace(doc.Timeout); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return limits{}, errdefs.Validationf("graph: limits.timeout: %w", err)
		}
		if parsed < 0 {
			return limits{}, errdefs.Validationf("graph: limits.timeout must not be negative")
		}
		out.timeout = parsed
	}
	return out, nil
}

// buildState validates the declared state slots.
func buildState(doc map[string]StateDocument) (map[string]StateKey, error) {
	state := make(map[string]StateKey, len(doc))
	for key, spec := range doc {
		if strings.TrimSpace(key) == "" {
			return nil, errdefs.Validationf("graph: a state key has an empty name")
		}
		kind := Kind(strings.TrimSpace(spec.Type))
		if !kind.Valid() {
			return nil, errdefs.Validationf("graph: state %q: type %q is not declared", key, spec.Type)
		}
		merge := Merge(strings.TrimSpace(spec.Merge))
		// The document spells the default out; the in-memory rule is the zero
		// value, so "single" is the one literal that is not a Merge constant.
		if merge == "single" {
			merge = MergeSingle
		}
		if !merge.Valid() {
			return nil, errdefs.Validationf("graph: state %q: merge %q is not declared", key, spec.Merge)
		}
		if merge == MergeAppend && kind != KindMessages {
			return nil, errdefs.Validationf("graph: state %q: merge: append needs type: messages", key)
		}
		state[key] = StateKey{Kind: kind, Merge: merge, Carry: spec.Carry}
	}
	return state, nil
}

// carriedKeys lists the keys a new task seeds from the session's previous one.
func carriedKeys(state map[string]StateKey) map[string]struct{} {
	carried := make(map[string]struct{})
	for key, spec := range state {
		if spec.Carry {
			carried[key] = struct{}{}
		}
	}
	return carried
}

// buildNode validates one node and decodes its config once.
func buildNode(index int, doc NodeDocument, registry *Registry, declared map[string]nodeSpec) (nodeSpec, error) {
	id := strings.TrimSpace(doc.ID)
	if id == "" {
		return nodeSpec{}, errdefs.Validationf("graph: nodes[%d]: id is required", index)
	}
	if _, duplicate := declared[id]; duplicate {
		return nodeSpec{}, errdefs.Validationf("graph: node %q is declared twice", id)
	}
	if id == End {
		return nodeSpec{}, errdefs.Validationf("graph: node %q is reserved", id)
	}
	kind, ok := registry.Lookup(strings.TrimSpace(doc.Kind))
	if !ok {
		return nodeSpec{}, errdefs.Validationf("graph: node %q: kind %q is not registered", id, doc.Kind)
	}
	spec := nodeSpec{id: id, kind: kind, config: doc.Config}
	if decoder, ok := kind.(ConfigDecoder); ok {
		decoded, err := decoder.DecodeConfig(doc.Config)
		if err != nil {
			return nodeSpec{}, errdefs.Validationf("graph: node %q config: %w", id, err)
		}
		spec.decoded = decoded
	}
	if raw := strings.TrimSpace(doc.Skip); raw != "" {
		compiled, err := CompileCondition(raw)
		if err != nil {
			return nodeSpec{}, errdefs.Validationf("graph: node %q skip: %w", id, err)
		}
		spec.skip = compiled
	}
	if raw := strings.TrimSpace(doc.Timeout); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nodeSpec{}, errdefs.Validationf("graph: node %q timeout: %w", id, err)
		}
		if parsed < 0 {
			return nodeSpec{}, errdefs.Validationf("graph: node %q timeout must not be negative", id)
		}
		spec.timeout = parsed
	}
	if doc.Retries < 0 {
		return nodeSpec{}, errdefs.Validationf("graph: node %q retries must not be negative", id)
	}
	spec.retries = doc.Retries
	return spec, nil
}

// wireEdges validates every transition and compiles its condition.
func (g *Graph) wireEdges(docs []EdgeDocument) error {
	for i, doc := range docs {
		from := strings.TrimSpace(doc.From)
		if _, ok := g.nodes[from]; !ok {
			return errdefs.Validationf("graph: edges[%d]: unknown node %q", i, from)
		}
		to := strings.TrimSpace(doc.To)
		if to != End {
			if _, ok := g.nodes[to]; !ok {
				return errdefs.Validationf("graph: edges[%d]: unknown target %q", i, to)
			}
		}
		wired := edge{to: to}
		if raw := strings.TrimSpace(doc.When); raw != "" {
			compiled, err := CompileCondition(raw)
			if err != nil {
				return errdefs.Validationf("graph: edges[%d]: %w", i, err)
			}
			wired.when = compiled
		}
		g.edges[from] = append(g.edges[from], wired)
	}
	return nil
}

// checkReachable refuses a node no path from the entry reaches: it can only be
// a wiring mistake, and at run time it would simply never fire.
func (g *Graph) checkReachable() error {
	seen := make(map[string]bool, len(g.nodes))
	queue := []string{g.entry}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		for _, outgoing := range g.edges[id] {
			if outgoing.to != End && !seen[outgoing.to] {
				queue = append(queue, outgoing.to)
			}
		}
	}
	for _, id := range g.order {
		if !seen[id] {
			return errdefs.Validationf("graph: node %q is unreachable from %q", id, g.entry)
		}
	}
	return nil
}

// wireOutput names the key a finished run reports.
func (g *Graph) wireOutput(doc OutputDocument) error {
	key := strings.TrimSpace(doc.Key)
	if key == "" {
		return errdefs.Validationf("graph: output: key is required")
	}
	if _, ok := g.state[key]; !ok {
		return errdefs.Validationf("graph: output: %q is not a declared state key", key)
	}
	g.output = key
	return nil
}
