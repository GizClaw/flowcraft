package derive

import (
	"context"
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/memory/component"
)

// Status is the durable execution state of one node.
type Status string

const (
	StatusSuccess Status = "success"
	StatusFailed  Status = "failed"
	StatusBlocked Status = "blocked"
)

// NodeResult is a checkpoint-friendly record of one node attempt. Err retains
// the in-process cause; Error is its serializable representation.
type NodeResult struct {
	ID        string               `json:"id"`
	Status    Status               `json:"status"`
	Artifacts []component.Artifact `json:"artifacts,omitempty"`
	Error     string               `json:"error,omitempty"`
	Err       error                `json:"-"`
	Attempt   int                  `json:"attempt"`
	BlockedBy []string             `json:"blocked_by,omitempty"`
	Cancelled bool                 `json:"cancelled,omitempty"`
}

// RunResult contains all state needed by a later checkpoint or worker layer.
type RunResult struct {
	Source component.Artifact `json:"source"`
	Nodes  []NodeResult       `json:"nodes"`
}

// Clone returns an independently owned run result.
func (result RunResult) Clone() RunResult {
	cloned := RunResult{
		Source: result.Source.Clone(),
		Nodes:  make([]NodeResult, len(result.Nodes)),
	}
	for index, item := range result.Nodes {
		item.Artifacts = component.CloneArtifacts(item.Artifacts)
		item.BlockedBy = append([]string(nil), item.BlockedBy...)
		cloned.Nodes[index] = item
	}
	return cloned
}

// Node returns an independently owned result for id.
func (result RunResult) Node(id string) (NodeResult, bool) {
	for _, item := range result.Nodes {
		if item.ID == id {
			item.Artifacts = component.CloneArtifacts(item.Artifacts)
			item.BlockedBy = append([]string(nil), item.BlockedBy...)
			return item, true
		}
	}
	return NodeResult{}, false
}

// Run executes nodes serially in deterministic topological order. Node-level
// failures are recorded in the result and do not become a top-level error.
func (dag *DAG) Run(ctx context.Context, source component.Artifact) (RunResult, error) {
	if dag == nil {
		return RunResult{}, errors.New("memory derive: DAG is required")
	}
	if ctx == nil {
		return RunResult{}, errors.New("memory derive: context is required")
	}
	if err := source.Validate(); err != nil {
		return RunResult{}, fmt.Errorf("memory derive: source: %w", err)
	}
	return dag.execute(ctx, RunResult{Source: source.Clone()}, nil), nil
}

// Retry re-executes failed nodes and blocked descendants only. Successful node
// outputs are reused without invoking their Derivers again.
func (dag *DAG) Retry(ctx context.Context, previous RunResult) (RunResult, error) {
	if dag == nil {
		return RunResult{}, errors.New("memory derive: DAG is required")
	}
	if ctx == nil {
		return RunResult{}, errors.New("memory derive: context is required")
	}
	if err := dag.validatePrevious(previous); err != nil {
		return RunResult{}, err
	}
	retry := dag.retrySet(previous)
	return dag.execute(ctx, previous.Clone(), retry), nil
}

// retry == nil denotes a fresh run. A non-nil map denotes retry mode.
func (dag *DAG) execute(ctx context.Context, base RunResult, retry map[string]bool) RunResult {
	previous := resultMap(base.Nodes)
	current := make(map[string]NodeResult, len(dag.order))
	result := RunResult{
		Source: base.Source.Clone(),
		Nodes:  make([]NodeResult, 0, len(dag.order)),
	}

	for _, id := range dag.order {
		if retry != nil && !retry[id] {
			reused := cloneNodeResult(previous[id])
			current[id] = reused
			result.Nodes = append(result.Nodes, reused)
			continue
		}

		item := dag.nodes[id]
		attempt := 1
		if prior, exists := previous[id]; exists {
			attempt = prior.Attempt + 1
		}
		blockedBy := failedDependencies(item.spec.DependsOn, current)
		if len(blockedBy) > 0 {
			nodeResult := NodeResult{
				ID:        id,
				Status:    StatusBlocked,
				Error:     "blocked by unsuccessful dependencies",
				Attempt:   attempt - 1,
				BlockedBy: blockedBy,
			}
			for _, dependency := range blockedBy {
				if current[dependency].Cancelled {
					nodeResult.Error = "blocked by cancelled dependencies"
					nodeResult.Cancelled = true
					break
				}
			}
			current[id] = nodeResult
			result.Nodes = append(result.Nodes, nodeResult)
			continue
		}
		if err := ctx.Err(); err != nil {
			nodeResult := cancelledResult(id, attempt-1, err)
			current[id] = nodeResult
			result.Nodes = append(result.Nodes, nodeResult)
			continue
		}

		inputs := dag.inputs(item.spec.DependsOn, result.Source, current)
		artifacts, err := invokeNode(ctx, id, item.deriver, item.factory.Ports.Outputs, inputs)
		if err != nil {
			nodeResult := NodeResult{
				ID:      id,
				Status:  StatusFailed,
				Error:   err.Error(),
				Err:     err,
				Attempt: attempt,
			}
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				nodeResult = cancelledResult(id, attempt, ctx.Err())
			}
			current[id] = nodeResult
			result.Nodes = append(result.Nodes, nodeResult)
			continue
		}
		nodeResult := NodeResult{
			ID:        id,
			Status:    StatusSuccess,
			Artifacts: component.CloneArtifacts(artifacts),
			Attempt:   attempt,
		}
		current[id] = nodeResult
		result.Nodes = append(result.Nodes, nodeResult)
	}
	return result
}

func (dag *DAG) inputs(dependencies []string, source component.Artifact, results map[string]NodeResult) []component.Artifact {
	if len(dependencies) == 0 {
		return []component.Artifact{source.Clone()}
	}
	var inputs []component.Artifact
	for _, dependency := range dependencies {
		inputs = append(inputs, component.CloneArtifacts(results[dependency].Artifacts)...)
	}
	return inputs
}

func invokeNode(ctx context.Context, id string, deriver component.Deriver, outputKinds []component.ArtifactKind, inputs []component.Artifact) ([]component.Artifact, error) {
	var aggregated []component.Artifact
	for inputIndex, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		outputs, err := deriver.Derive(ctx, input.Clone())
		if err != nil {
			return nil, fmt.Errorf("memory derive: node %q input %d: %w", id, inputIndex, err)
		}
		for outputIndex, output := range outputs {
			if err := output.Validate(); err != nil {
				return nil, fmt.Errorf("memory derive: node %q input %d output %d: %w", id, inputIndex, outputIndex, err)
			}
			if len(outputKinds) > 0 && !containsKind(outputKinds, output.Kind) {
				return nil, fmt.Errorf(
					"memory derive: node %q input %d output %d kind %q is not declared in %v",
					id, inputIndex, outputIndex, output.Kind, outputKinds,
				)
			}
			aggregated = append(aggregated, output.Clone())
		}
	}
	return aggregated, nil
}

func failedDependencies(dependencies []string, results map[string]NodeResult) []string {
	var blocked []string
	for _, dependency := range dependencies {
		if results[dependency].Status != StatusSuccess {
			blocked = append(blocked, dependency)
		}
	}
	return blocked
}

func cancelledResult(id string, attempt int, err error) NodeResult {
	return NodeResult{
		ID:        id,
		Status:    StatusBlocked,
		Error:     err.Error(),
		Err:       err,
		Attempt:   attempt,
		Cancelled: true,
	}
}

func resultMap(results []NodeResult) map[string]NodeResult {
	byID := make(map[string]NodeResult, len(results))
	for _, item := range results {
		byID[item.ID] = item
	}
	return byID
}

func cloneNodeResult(item NodeResult) NodeResult {
	item.Artifacts = component.CloneArtifacts(item.Artifacts)
	item.BlockedBy = append([]string(nil), item.BlockedBy...)
	return item
}

func (dag *DAG) validatePrevious(previous RunResult) error {
	if err := previous.Source.Validate(); err != nil {
		return fmt.Errorf("memory derive: previous source: %w", err)
	}
	if len(previous.Nodes) != len(dag.order) {
		return errors.New("memory derive: previous result does not match DAG")
	}
	seen := make(map[string]struct{}, len(previous.Nodes))
	for index, item := range previous.Nodes {
		if item.ID != dag.order[index] {
			return errors.New("memory derive: previous result order does not match DAG")
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return fmt.Errorf("memory derive: previous result duplicates node %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		switch item.Status {
		case StatusSuccess:
			for artifactIndex, artifact := range item.Artifacts {
				if err := artifact.Validate(); err != nil {
					return fmt.Errorf("memory derive: previous node %q artifact %d: %w", item.ID, artifactIndex, err)
				}
			}
		case StatusFailed, StatusBlocked:
		default:
			return fmt.Errorf("memory derive: previous node %q has invalid status %q", item.ID, item.Status)
		}
	}
	return nil
}

func (dag *DAG) retrySet(previous RunResult) map[string]bool {
	retry := make(map[string]bool)
	for _, item := range previous.Nodes {
		if item.Status == StatusFailed || (item.Status == StatusBlocked && item.Cancelled) {
			retry[item.ID] = true
		}
	}
	changed := true
	for changed {
		changed = false
		for _, id := range dag.order {
			if retry[id] {
				continue
			}
			prior, _ := previous.Node(id)
			if prior.Status != StatusBlocked {
				continue
			}
			for _, dependency := range dag.nodes[id].spec.DependsOn {
				if retry[dependency] {
					retry[id] = true
					changed = true
					break
				}
			}
		}
	}
	return retry
}
