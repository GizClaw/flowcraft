package bindings

import (
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// The catalog reads: the two entry points that answer questions about
// the host's registered models instead of running one.

// models is inference.models: the full catalog of ModelDescriptor wire
// JSON (id, capabilities, limits, lifecycle).
//
// It takes no argument — the script runtimes bind a Go func by its arity,
// so this stays nullary while every other entry point takes the request
// object.
func (b *inferenceBridge) models() (any, error) {
	const op = "inference.models"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	return toScriptJSON(b.assembly.Models(), op+" response")
}

// inspect is inference.inspect: one ModelRef in, that model's
// ModelDescriptor out.
func (b *inferenceBridge) inspect(raw any) (any, error) {
	const op = "inference.inspect"
	if err := b.requireAssembly(op); err != nil {
		return nil, err
	}
	var ref model.ModelRef
	if err := decodeStrictJSON(raw, &ref, op+".model"); err != nil {
		return nil, err
	}
	descriptor, err := b.assembly.InspectModel(ref)
	if err != nil {
		return nil, err
	}
	return toScriptJSON(descriptor, op+" response")
}
