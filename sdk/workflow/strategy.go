package workflow

import (
	"context"
	"fmt"
)

// StrategyCapabilities describes how Runtime reads outputs from the Board.
type StrategyCapabilities struct {
	AnswerKey string
}

// AnswerVar returns the board var key for the final answer, defaulting to "answer".
func (c StrategyCapabilities) AnswerVar() string {
	if c.AnswerKey != "" {
		return c.AnswerKey
	}
	return VarAnswer
}

// Strategy describes how to execute one turn (graph, script, remote, …).
type Strategy interface {
	Kind() string
	Build(ctx context.Context, deps *Dependencies) (Runnable, error)
	Capabilities() StrategyCapabilities
}

// Runnable is the compiled execution unit produced by Strategy.Build.
type Runnable interface {
	Execute(ctx context.Context, board *Board, req *Request, opts ...RunOption) (*Board, error)
}

// Dependencies is a type-safe container for resources available to Strategy.Build.
// Each Strategy defines its own key constants and retrieves values via GetDep.
type Dependencies struct {
	store map[string]any
}

// NewDependencies creates an empty Dependencies container.
func NewDependencies() *Dependencies {
	return &Dependencies{store: make(map[string]any)}
}

// Set stores a dependency value under the given key.
func (d *Dependencies) Set(key string, val any) {
	if d.store == nil {
		d.store = make(map[string]any)
	}
	d.store[key] = val
}

// SetDep stores a typed dependency value. The type parameter is for documentation
// only at the call site; retrieval is type-checked by GetDep.
func SetDep[T any](d *Dependencies, key string, val T) {
	d.Set(key, val)
}

// GetDep retrieves a typed dependency. It returns an error if the key is missing
// or the stored value does not match the requested type.
func GetDep[T any](d *Dependencies, key string) (T, error) {
	if d == nil || d.store == nil {
		var zero T
		return zero, fmt.Errorf("dependency %q not found (nil container)", key)
	}
	raw, ok := d.store[key]
	if !ok {
		var zero T
		return zero, fmt.Errorf("dependency %q not found", key)
	}
	val, ok := raw.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("dependency %q: want %T, got %T", key, zero, raw)
	}
	return val, nil
}
