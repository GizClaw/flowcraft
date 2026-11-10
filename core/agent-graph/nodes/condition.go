package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent-graph"
)

// Condition returns the "condition" node kind: it evaluates an expression over
// the state and writes one of two values.
//
// config:
//
//	key:  state key to write (type text), required
//	when: expression over the state, required
//	then: value when the expression holds
//	else: value otherwise
//
// It exists so an edge can match a plain value (`route == "long"`) instead of
// repeating a decision in every condition. Edge conditions can still read the
// state directly; this kind is for a decision that is worth naming once.
type conditionKind struct {
	mu       sync.Mutex
	compiled map[string]*graph.CompiledCondition
}

type conditionConfig struct {
	Key  string `json:"key"`
	When string `json:"when"`
	Then string `json:"then"`
	Else string `json:"else"`
}

// DecodeConfig parses the node's config at Build.
func (*conditionKind) DecodeConfig(raw json.RawMessage) (any, error) {
	return decodeConditionConfig(raw)
}

func decodeConditionConfig(raw json.RawMessage) (conditionConfig, error) {
	var config conditionConfig
	if err := graph.DecodeConfig(raw, &config); err != nil {
		return conditionConfig{}, err
	}
	return config, nil
}

// Condition builds the condition kind.
func Condition() graph.NodeKind {
	return &conditionKind{compiled: make(map[string]*graph.CompiledCondition)}
}

func (*conditionKind) Name() string { return "condition" }

func (c *conditionKind) Invoke(_ context.Context, in graph.Invoke) (graph.NodeStream, error) {
	config, err := graph.ConfigOf(in, decodeConditionConfig)
	if err != nil {
		return nil, fmt.Errorf("condition: %w", err)
	}
	if strings.TrimSpace(config.Key) == "" || strings.TrimSpace(config.When) == "" {
		return nil, fmt.Errorf("condition: config.key and config.when are required")
	}
	compiled, err := c.compile(config.When)
	if err != nil {
		return nil, err
	}
	holds, err := compiled.Evaluate(in.Step, in.Snapshot())
	if err != nil {
		return nil, fmt.Errorf("condition: %w", err)
	}
	value := config.Else
	if holds {
		value = config.Then
	}
	return graph.Static(graph.Result{State: []graph.StateUpdate{
		{Key: config.Key, Value: graph.Text(value)},
	}}), nil
}

// compile caches by expression: the same document evaluates the same source on
// every step, and compiling once per step would be work with no result.
func (c *conditionKind) compile(raw string) (*graph.CompiledCondition, error) {
	// Parallel branches of one step invoke the same kind instance, so the cache
	// needs a lock: a bare map write here is a data race and a panic.
	c.mu.Lock()
	defer c.mu.Unlock()
	if compiled, ok := c.compiled[raw]; ok {
		return compiled, nil
	}
	compiled, err := graph.CompileCondition(raw)
	if err != nil {
		return nil, fmt.Errorf("condition: %w", err)
	}
	c.compiled[raw] = compiled
	return compiled, nil
}
