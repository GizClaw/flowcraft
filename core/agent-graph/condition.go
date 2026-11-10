package graph

import (
	"encoding/json"
	"fmt"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// CompiledCondition is a boolean expression over the state, compiled once at
// Build time so a step pays only evaluation.
//
// The environment is the state as it was when the step started plus "step":
// an expression such as `pending != nil and step < 3` reads values by the names
// the document declared. Compiling at build turns a typo into a load error
// instead of a run-time surprise.
type CompiledCondition struct {
	// Raw is the source, kept for diagnostics and for reading a document.
	Raw     string
	program *vm.Program
}

func CompileCondition(raw string) (*CompiledCondition, error) {
	program, err := expr.Compile(raw, expr.AsBool())
	if err != nil {
		return nil, errdefs.Validationf("graph: invalid condition %q: %w", raw, err)
	}
	return &CompiledCondition{Raw: raw, program: program}, nil
}

// Evaluate runs the condition against one step's view of the state.
func (c *CompiledCondition) Evaluate(step int, snapshot Snapshot) (bool, error) {
	return c.EvaluateEnv(Env(step, snapshot))
}

// EvaluateEnv runs the condition against an environment already built by [Env].
//
// A step evaluates its skip conditions and then its edges against the same
// snapshot, so building the projection once per step instead of once per
// condition removes a map and a dozen allocations from every evaluation.
func (c *CompiledCondition) EvaluateEnv(env map[string]any) (bool, error) {
	out, err := expr.Run(c.program, env)
	if err != nil {
		return false, fmt.Errorf("graph: condition %q: %w", c.Raw, err)
	}
	value, ok := out.(bool)
	if !ok {
		return false, fmt.Errorf("graph: condition %q returned %T, want bool", c.Raw, out)
	}
	return value, nil
}

// Env renders the state for evaluation. Values keep their shape: text is
// a string, a number or object is decoded JSON, a session is the message
// slice so `len(messages)` works, media is its descriptor.
func Env(step int, snapshot Snapshot) map[string]any {
	state := make(map[string]any, snapshot.Len())
	snapshot.Range(func(key string, value Value) bool {
		state[key] = Plain(value)
		return true
	})
	// Keys live under "state": at the top level a key named "count" or "len"
	// would resolve to expr-lang's builtin instead of the value.
	return map[string]any{"state": state, "step": step}
}

// Plain converts one state value into what an expression sees.
func Plain(value Value) any {
	switch value.Kind() {
	case KindText:
		text, _ := value.Text()
		return text
	case KindBool:
		flag, _ := value.Bool()
		return flag
	case KindJSON:
		raw, _ := value.JSON()
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err == nil {
			return decoded
		}
		return string(raw)
	case KindMessages:
		messages, _ := value.Messages()
		return messages.Slice()
	case KindImage, KindAudio, KindFile, KindVideo:
		media, _ := value.Media()
		return map[string]any{
			"media_type": media.MediaType,
			"blob":       media.Blob,
			"size":       len(media.Data),
		}
	}
	return nil
}
