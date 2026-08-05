package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	observationview "github.com/GizClaw/flowcraft/memory/views/observation"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// Phase identifies a built-in lifecycle operation.
type Phase string

const (
	PhaseIntegrate Phase = "integrate"
	PhaseCompact   Phase = "compact"
	PhaseDecay     Phase = "decay"
	PhaseForget    Phase = "forget"
	PhaseRepair    Phase = "repair"
)

// StateKind is a typed lifecycle state capability consumed or produced by a node.
type StateKind string

const (
	StateTask         StateKind = "task"
	StateFact         StateKind = "fact"
	StateObservations StateKind = "observations"
	StateScores       StateKind = "scores"
	StateForgetPlan   StateKind = "forget_plan"
	StateRepairPlan   StateKind = "repair_plan"
	StateEffectSink   StateKind = "effect_sink"
)

// Step executes one lifecycle node against typed state.
type Step interface {
	Run(context.Context, *RunState) error
}

// Effect is a scoped custom lifecycle side effect. Scope is intentionally
// omitted: the executor-bound sink supplies it from RunState.Task().
type Effect struct {
	Key   string
	Value []byte
}

// EffectSink is the only general side-effect capability exposed to custom steps.
type EffectSink interface {
	Emit(context.Context, Effect) error
}

// Services exposes only capabilities safe for custom steps. Built-in steps use
// private callbacks populated by Service.
type Services struct {
	Effects EffectSink

	integrate func(context.Context, *RunState) error
	compact   func(context.Context, *RunState) error
	decay     func(context.Context, *RunState) error
	forget    func(context.Context, *RunState) error
	repair    func(context.Context, *RunState) error
}

// RunState carries typed lifecycle artifacts.
type RunState struct {
	NodeID       string
	task         Task
	fact         factview.Fact
	Observation  *observationview.Observation
	Observations []observationview.Observation
	Scores       []ScoreSnapshot
	ForgetPlan   *ForgetPlan
	RepairPlan   *RepairPlan
	Services     Services
}

// Task returns the immutable lifecycle task by value.
func (state *RunState) Task() Task {
	if state == nil {
		return Task{}
	}
	return state.task
}

// Fact returns an owned clone. A custom step cannot mutate canonical content
// or influence later built-in phases through aliases.
func (state *RunState) Fact() factview.Fact {
	if state == nil {
		return factview.Fact{}
	}
	return cloneLifecycleFact(state.fact)
}

// StepContract declares typed state requirements and outputs for custom nodes.
type StepContract struct {
	Requires []StateKind `json:"requires,omitempty"`
	Produces []StateKind `json:"produces,omitempty"`
}

// StepSpec is an opaque typed factory selection.
type StepSpec struct {
	name       string
	config     any
	configType reflect.Type
}

func NewStepSpec[C any](name string, config C) StepSpec {
	return StepSpec{name: name, config: config, configType: reflect.TypeFor[C]()}
}

func NewUnconfiguredStepSpec(name string) StepSpec {
	return StepSpec{name: name}
}

func (spec StepSpec) FactoryName() string { return spec.name }

func (spec StepSpec) CanonicalConfig() ([]byte, error) {
	if spec.configType == nil {
		return []byte("null"), nil
	}
	return json.Marshal(spec.config)
}

type stepFactory struct {
	version    string
	configType reflect.Type
	contract   StepContract
	build      func(any) (Step, error)
}

// Catalog stores programmatic typed lifecycle factories.
type Catalog struct {
	mu        sync.RWMutex
	factories map[string]stepFactory
}

func NewCatalog() *Catalog { return &Catalog{factories: make(map[string]stepFactory)} }

func (catalog *Catalog) Clone() *Catalog {
	if catalog == nil {
		return NewCatalog()
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	cloned := NewCatalog()
	for name, factory := range catalog.factories {
		factory.contract = cloneContract(factory.contract)
		cloned.factories[name] = factory
	}
	return cloned
}

// RegisterTypedStep registers a concrete Go configuration type.
func RegisterTypedStep[C any](catalog *Catalog, name, version string, contract StepContract, factory func(C) (Step, error)) error {
	if catalog == nil || factory == nil {
		return errors.New("memory lifecycle: catalog and step factory are required")
	}
	if err := validateNodeID(name); err != nil {
		return err
	}
	if strings.TrimSpace(version) == "" {
		return errors.New("memory lifecycle: step factory version is required")
	}
	typ := reflect.TypeFor[C]()
	if typ.Kind() != reflect.Struct {
		return fmt.Errorf("memory lifecycle: step config must be a concrete struct, got %v", typ)
	}
	if err := validateContract(contract); err != nil {
		return err
	}
	entry := stepFactory{
		version: version, configType: typ, contract: cloneContract(contract),
		build: func(value any) (Step, error) { return factory(value.(C)) },
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if _, exists := catalog.factories[name]; exists {
		return fmt.Errorf("memory lifecycle: step factory %q already registered", name)
	}
	catalog.factories[name] = entry
	return nil
}

type NodeSpec struct {
	ID        string
	Phase     Phase
	Factory   StepSpec
	DependsOn []string
}

type Spec struct {
	Nodes []NodeSpec
}

type builtNode struct {
	spec     NodeSpec
	step     Step
	version  string
	config   []byte
	contract StepContract
}

// DAG is an immutable executable lifecycle graph.
type DAG struct {
	nodes  []builtNode
	order  []int
	digest string
}

func (dag *DAG) TopologicalOrder() []string {
	if dag == nil {
		return nil
	}
	result := make([]string, len(dag.order))
	for index, nodeIndex := range dag.order {
		result[index] = dag.nodes[nodeIndex].spec.ID
	}
	return result
}

func (dag *DAG) Digest() string {
	if dag == nil {
		return ""
	}
	return dag.digest
}

// Build validates topology and typed state flow before constructing a DAG.
func Build(catalog *Catalog, spec Spec, available ...StateKind) (*DAG, error) {
	if len(spec.Nodes) == 0 {
		spec = DefaultSpec()
	}
	catalog = catalog.Clone()
	nodes := make([]builtNode, len(spec.Nodes))
	indexByID := make(map[string]int, len(spec.Nodes))
	for index, configured := range spec.Nodes {
		if err := validateNodeID(configured.ID); err != nil {
			return nil, fmt.Errorf("memory lifecycle: nodes[%d]: %w", index, err)
		}
		if _, exists := indexByID[configured.ID]; exists {
			return nil, fmt.Errorf("memory lifecycle: duplicate node %q", configured.ID)
		}
		indexByID[configured.ID] = index
		selected := 0
		if configured.Phase != "" {
			selected++
		}
		if configured.Factory.name != "" {
			selected++
		}
		if selected != 1 {
			return nil, fmt.Errorf("memory lifecycle: node %q must select exactly one phase or typed factory", configured.ID)
		}
		node := builtNode{spec: configured}
		if configured.Phase != "" {
			contract, ok := builtinContract(configured.Phase)
			if !ok {
				return nil, fmt.Errorf("memory lifecycle: node %q has unknown phase %q", configured.ID, configured.Phase)
			}
			node.step = phaseStep{phase: configured.Phase}
			node.version = builtinVersion(configured.Phase)
			node.config = []byte("null")
			node.contract = contract
		} else {
			resolved, err := catalog.resolve(configured.Factory)
			if err != nil {
				return nil, fmt.Errorf("memory lifecycle: node %q: %w", configured.ID, err)
			}
			node = builtNode{
				spec: configured, step: resolved.step, version: resolved.version,
				config: resolved.config, contract: resolved.contract,
			}
		}
		for _, produced := range node.contract.Produces {
			if produced == StateTask {
				return nil, fmt.Errorf("memory lifecycle: node %q cannot produce immutable task state", configured.ID)
			}
		}
		nodes[index] = node
	}
	indegree := make([]int, len(nodes))
	children := make([][]int, len(nodes))
	for index, node := range nodes {
		seen := map[string]struct{}{}
		for _, dependency := range node.spec.DependsOn {
			dependencyIndex, exists := indexByID[dependency]
			if !exists {
				return nil, fmt.Errorf("memory lifecycle: node %q depends on missing node %q", node.spec.ID, dependency)
			}
			if _, duplicate := seen[dependency]; duplicate {
				return nil, fmt.Errorf("memory lifecycle: node %q repeats dependency %q", node.spec.ID, dependency)
			}
			seen[dependency] = struct{}{}
			indegree[index]++
			children[dependencyIndex] = append(children[dependencyIndex], index)
		}
	}
	var ready []int
	for index, degree := range indegree {
		if degree == 0 {
			ready = append(ready, index)
		}
	}
	var order []int
	for len(ready) > 0 {
		index := ready[0]
		ready = ready[1:]
		order = append(order, index)
		for _, child := range children[index] {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
				sort.Ints(ready)
			}
		}
	}
	if len(order) != len(nodes) {
		return nil, errors.New("memory lifecycle: DAG contains a cycle")
	}
	if err := validateStateFlow(nodes, order, indexByID, available); err != nil {
		return nil, err
	}
	dag := &DAG{nodes: nodes, order: order}
	dag.digest = dagDigest(nodes)
	return dag, nil
}

func DefaultSpec() Spec {
	return Spec{Nodes: []NodeSpec{
		{ID: "integrate", Phase: PhaseIntegrate},
		{ID: "compact", Phase: PhaseCompact, DependsOn: []string{"integrate"}},
		{ID: "decay", Phase: PhaseDecay, DependsOn: []string{"compact"}},
		{ID: "forget", Phase: PhaseForget, DependsOn: []string{"decay"}},
		{ID: "repair", Phase: PhaseRepair, DependsOn: []string{"forget"}},
	}}
}

func (dag *DAG) Run(ctx context.Context, state *RunState, checkpoints CheckpointStore) error {
	if dag == nil || state == nil || checkpoints == nil {
		return errors.New("memory lifecycle: DAG, run state, and checkpoints are required")
	}
	if err := state.task.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(state.task.PublicationID) == "" || strings.TrimSpace(state.task.PolicyDigest) == "" ||
		strings.TrimSpace(state.task.Branch) == "" {
		return errors.New("memory lifecycle: publication, policy, and branch are required")
	}
	for _, index := range dag.order {
		if err := ctx.Err(); err != nil {
			return err
		}
		node := dag.nodes[index]
		key := CheckpointKey{
			Scope: state.task.Scope, PublicationID: state.task.PublicationID,
			Node: node.spec.ID, Branch: state.task.Branch, PolicyDigest: state.task.PolicyDigest, DAGDigest: dag.digest,
		}
		checkpoint, found, err := checkpoints.Load(ctx, key)
		if err != nil {
			return err
		}
		if found && checkpoint.Status == CheckpointCompleted {
			restoreCheckpointState(state, checkpoint.State)
			continue
		}
		state.NodeID = node.spec.ID
		if err := node.step.Run(ctx, state); err != nil {
			_ = checkpoints.Save(context.WithoutCancel(ctx), Checkpoint{Key: key, Status: CheckpointError, Error: err.Error()})
			return fmt.Errorf("memory lifecycle: node %q: %w", node.spec.ID, err)
		}
		if err := validateProducedState(node, state); err != nil {
			_ = checkpoints.Save(context.WithoutCancel(ctx), Checkpoint{Key: key, Status: CheckpointError, Error: err.Error()})
			return err
		}
		if err := checkpoints.Save(ctx, Checkpoint{
			Key: key, Status: CheckpointCompleted, State: captureCheckpointState(state),
		}); err != nil {
			return err
		}
	}
	return nil
}

func validateProducedState(node builtNode, state *RunState) error {
	for _, produced := range node.contract.Produces {
		present := false
		switch produced {
		case StateFact:
			present = state.fact.ID != ""
		case StateObservations:
			present = state.Observations != nil
		case StateScores:
			present = state.Scores != nil
		case StateForgetPlan:
			present = state.ForgetPlan != nil
		case StateRepairPlan:
			present = state.RepairPlan != nil
		case StateEffectSink:
			present = state.Services.Effects != nil
		}
		if !present {
			return fmt.Errorf("memory lifecycle: node %q did not produce required state %q", node.spec.ID, produced)
		}
	}
	return nil
}

func captureCheckpointState(state *RunState) *CheckpointState {
	result := &CheckpointState{
		Observations: cloneObservations(state.Observations),
		Scores:       append([]ScoreSnapshot(nil), state.Scores...),
	}
	for index := range result.Scores {
		result.Scores[index].SourceRefs = append([]sdkmemory.SourceRef(nil), result.Scores[index].SourceRefs...)
	}
	if state.Observation != nil {
		value := cloneObservation(*state.Observation)
		result.Observation = &value
	}
	if state.ForgetPlan != nil {
		value := *state.ForgetPlan
		value.Candidates = append([]ForgetCandidate(nil), value.Candidates...)
		result.ForgetPlan = &value
	}
	if state.RepairPlan != nil {
		value := *state.RepairPlan
		value.Actions = append([]RepairAction(nil), value.Actions...)
		result.RepairPlan = &value
	}
	return result
}

func restoreCheckpointState(state *RunState, checkpoint *CheckpointState) {
	if checkpoint == nil {
		return
	}
	if checkpoint.Observation != nil {
		value := cloneObservation(*checkpoint.Observation)
		state.Observation = &value
	}
	if checkpoint.Observations != nil {
		state.Observations = cloneObservations(checkpoint.Observations)
	}
	if checkpoint.Scores != nil {
		state.Scores = append([]ScoreSnapshot(nil), checkpoint.Scores...)
		for index := range state.Scores {
			state.Scores[index].SourceRefs = append([]sdkmemory.SourceRef(nil), state.Scores[index].SourceRefs...)
		}
	}
	if checkpoint.ForgetPlan != nil {
		value := *checkpoint.ForgetPlan
		value.Candidates = append([]ForgetCandidate(nil), value.Candidates...)
		state.ForgetPlan = &value
	}
	if checkpoint.RepairPlan != nil {
		value := *checkpoint.RepairPlan
		value.Actions = append([]RepairAction(nil), value.Actions...)
		state.RepairPlan = &value
	}
}

type phaseStep struct{ phase Phase }

func (step phaseStep) Run(ctx context.Context, state *RunState) error {
	var run func(context.Context, *RunState) error
	switch step.phase {
	case PhaseIntegrate:
		run = state.Services.integrate
	case PhaseCompact:
		run = state.Services.compact
	case PhaseDecay:
		run = state.Services.decay
	case PhaseForget:
		run = state.Services.forget
	case PhaseRepair:
		run = state.Services.repair
	}
	if run == nil {
		return fmt.Errorf("memory lifecycle: phase %q service is unavailable", step.phase)
	}
	return run(ctx, state)
}

type resolvedStep struct {
	step     Step
	version  string
	config   []byte
	contract StepContract
}

func (catalog *Catalog) resolve(spec StepSpec) (resolvedStep, error) {
	if err := validateNodeID(spec.name); err != nil {
		return resolvedStep{}, err
	}
	catalog.mu.RLock()
	entry, exists := catalog.factories[spec.name]
	catalog.mu.RUnlock()
	if !exists {
		return resolvedStep{}, fmt.Errorf("step factory %q not found", spec.name)
	}
	if spec.configType == nil && entry.configType == reflect.TypeFor[struct{}]() {
		spec.config, spec.configType = struct{}{}, entry.configType
	}
	if spec.configType != entry.configType {
		return resolvedStep{}, fmt.Errorf("step factory %q config type %v does not match %v", spec.name, spec.configType, entry.configType)
	}
	data, err := json.Marshal(spec.config)
	if err != nil {
		return resolvedStep{}, err
	}
	cloned := reflect.New(entry.configType)
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cloned.Interface()); err != nil {
		return resolvedStep{}, err
	}
	step, err := entry.build(cloned.Elem().Interface())
	if err != nil {
		return resolvedStep{}, err
	}
	if step == nil {
		return resolvedStep{}, fmt.Errorf("step factory %q returned nil", spec.name)
	}
	return resolvedStep{step: step, version: entry.version, config: data, contract: cloneContract(entry.contract)}, nil
}

func builtinContract(phase Phase) (StepContract, bool) {
	switch phase {
	case PhaseIntegrate:
		return StepContract{Requires: []StateKind{StateTask, StateFact}, Produces: []StateKind{StateObservations}}, true
	case PhaseCompact:
		return StepContract{Requires: []StateKind{StateTask, StateFact}}, true
	case PhaseDecay:
		return StepContract{Requires: []StateKind{StateTask, StateObservations}, Produces: []StateKind{StateScores}}, true
	case PhaseForget:
		return StepContract{Requires: []StateKind{StateTask, StateScores}, Produces: []StateKind{StateForgetPlan}}, true
	case PhaseRepair:
		return StepContract{Requires: []StateKind{StateTask, StateFact}, Produces: []StateKind{StateRepairPlan}}, true
	default:
		return StepContract{}, false
	}
}

func builtinVersion(phase Phase) string {
	switch phase {
	case PhaseDecay:
		return DecayAlgorithmVersion
	case PhaseRepair:
		return RepairAlgorithmVersion
	default:
		return "lifecycle-" + string(phase) + "-v1"
	}
}

func validateStateFlow(nodes []builtNode, order []int, indexByID map[string]int, initial []StateKind) error {
	for _, index := range order {
		available := map[StateKind]struct{}{StateTask: {}, StateFact: {}}
		for _, value := range initial {
			if !validStateKind(value) {
				return fmt.Errorf("memory lifecycle: invalid available state %q", value)
			}
			available[value] = struct{}{}
		}
		var visit func(int)
		seen := map[int]struct{}{}
		visit = func(current int) {
			for _, dependency := range nodes[current].spec.DependsOn {
				dependencyIndex := indexByID[dependency]
				if _, ok := seen[dependencyIndex]; ok {
					continue
				}
				seen[dependencyIndex] = struct{}{}
				for _, produced := range nodes[dependencyIndex].contract.Produces {
					available[produced] = struct{}{}
				}
				visit(dependencyIndex)
			}
		}
		visit(index)
		for _, required := range nodes[index].contract.Requires {
			if _, ok := available[required]; !ok {
				return fmt.Errorf("memory lifecycle: node %q requires unavailable state %q", nodes[index].spec.ID, required)
			}
		}
	}
	return nil
}

func validateContract(contract StepContract) error {
	for group, values := range [][]StateKind{contract.Requires, contract.Produces} {
		seen := map[StateKind]struct{}{}
		for _, value := range values {
			if !validStateKind(value) {
				return fmt.Errorf("memory lifecycle: invalid state kind %q", value)
			}
			if group == 1 && value == StateTask {
				return errors.New("memory lifecycle: immutable task state cannot be produced")
			}
			if _, exists := seen[value]; exists {
				return fmt.Errorf("memory lifecycle: duplicate state kind %q", value)
			}
			seen[value] = struct{}{}
		}
	}
	return nil
}

func validStateKind(value StateKind) bool {
	switch value {
	case StateTask, StateFact, StateObservations, StateScores, StateForgetPlan, StateRepairPlan, StateEffectSink:
		return true
	default:
		return false
	}
}

func validateNodeID(value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 128 {
		return fmt.Errorf("invalid node or factory name %q", value)
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-' {
			continue
		}
		return fmt.Errorf("invalid node or factory name %q", value)
	}
	return nil
}

func cloneContract(value StepContract) StepContract {
	return StepContract{
		Requires: append([]StateKind(nil), value.Requires...),
		Produces: append([]StateKind(nil), value.Produces...),
	}
}

func dagDigest(nodes []builtNode) string {
	type digestNode struct {
		ID        string          `json:"id"`
		Phase     Phase           `json:"phase,omitempty"`
		Factory   string          `json:"factory,omitempty"`
		Version   string          `json:"version"`
		Config    json.RawMessage `json:"config"`
		DependsOn []string        `json:"depends_on,omitempty"`
		Contract  StepContract    `json:"contract"`
	}
	values := make([]digestNode, len(nodes))
	for index, node := range nodes {
		values[index] = digestNode{
			ID: node.spec.ID, Phase: node.spec.Phase, Factory: node.spec.Factory.name,
			Version: node.version, Config: node.config,
			DependsOn: append([]string(nil), node.spec.DependsOn...), Contract: cloneContract(node.contract),
		}
		sort.Strings(values[index].DependsOn)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	data, _ := json.Marshal(values)
	sum := sha256.Sum256(append([]byte("flowcraft.memory.lifecycle.dag\x00v1\x00"), data...))
	return hex.EncodeToString(sum[:])
}
