package protocol

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/core/inference"
)

// Shape is the execution contract a compilation targets. A provider may accept
// different fields for a unary call than for a streaming one, so the compiler is
// told which it is preparing.
type Shape string

const (
	// ShapeUnary is one request that yields one response.
	ShapeUnary Shape = "unary"
	// ShapeStream is one request that yields incremental events.
	ShapeStream Shape = "stream"
)

// Validate reports whether the shape is one the pipeline knows.
func (s Shape) Validate() error {
	switch s {
	case ShapeUnary, ShapeStream:
		return nil
	default:
		return errors.New("unknown execution shape " + string(s))
	}
}

// Compiled is one provider-native request plus the accounting that makes a
// dropped option detectable.
type Compiled[Wire any] struct {
	// Wire is the provider's own request value. Transport is its only reader:
	// it must not embed canonical request types, or a transport could observe a
	// canonical field that Fields never accounted for.
	Wire Wire
	// Fields names every canonical field this compilation honored. It must be a
	// subset of the request's active fields; together with Warnings it must
	// cover all of them.
	Fields []inference.Field
	// Warnings reports requested options honored only in part — an effort level
	// folded onto the nearest supported one, a default substituted for a value
	// the provider does not accept.
	Warnings []inference.Warning
}

// Compiler turns a canonical request into a provider-native request. It may
// validate and transform locally, and must not perform provider I/O: that is
// Transport's job, and keeping them apart is what makes a driver testable
// without a network.
//
// Implementations must be safe for concurrent use.
type Compiler[Req Request, Wire any] func(
	ctx context.Context,
	request Req,
	shape Shape,
) (Compiled[Wire], error)

// Request is what a pipeline compiles. Every workload's request type exposes
// the same two facts, which is what lets one contract check serve them all:
// which canonical fields it set, and whether it is valid at all.
type Request interface {
	// ActiveFields names the canonical fields this request uses. The contract
	// check requires the compilation to honour or explain every one.
	ActiveFields() []inference.Field
	// Validate rejects a request before any provider I/O.
	Validate() error
}

// contractError builds a driver-bug rejection that names the field.
func contractError(field inference.Field, detail string) *inference.Error {
	return inference.NewError(
		inference.CompilerContractViolation,
		field,
		errors.New(detail),
	).WithDetail(detail)
}

// checkContract verifies that the compilation accounted for every active field
// and invented none.
func checkContract[Req Request](
	request Req,
	fields []inference.Field,
	warnings []inference.Warning,
) error {
	active := request.ActiveFields()
	activeSet := make(map[inference.Field]struct{}, len(active))
	for _, field := range active {
		activeSet[field] = struct{}{}
	}

	explained := make(map[inference.Field]struct{}, len(fields)+len(warnings))
	for _, field := range fields {
		if _, ok := activeSet[field]; !ok {
			return contractError(field, "compiled a field the request did not set")
		}
		explained[field] = struct{}{}
	}
	for _, warning := range warnings {
		if warning.Field == "" {
			continue
		}
		explained[warning.Field] = struct{}{}
	}

	for _, field := range active {
		if _, ok := explained[field]; !ok {
			return contractError(field, "was requested but the compilation neither honored nor explained it")
		}
	}
	return nil
}
