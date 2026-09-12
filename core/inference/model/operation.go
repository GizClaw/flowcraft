package model

import (
	"fmt"
	"slices"
)

// Operation identifies one inference workload. Only workloads with an
// implemented request/response surface are enumerated here.
//
// Nothing is reserved ahead of implementation: a workload that has no
// request/response surface cannot be declared, routed, or advertised. The
// module previously carried a reserved "realtime" value plus ten ledger
// constants for it, which nothing produced or consumed; both were removed when
// this vocabulary became the single definition, because the module's own
// contract says providers must not advertise realtime until its surface ships.
// When realtime lands it adds one line here.
type Operation string

const (
	OperationGenerate      Operation = "generate"
	OperationEmbed         Operation = "embed"
	OperationTranscription Operation = "transcription"
)

// Operations returns every workload this vocabulary declares, in declaration
// order. It is the module's single enumeration of the operation axis: the
// tables that route, retry, and validation are written against are expected to
// cover it, and tests pin that they do, so adding a workload cannot silently
// leave one table behind.
func Operations() []Operation {
	return []Operation{OperationGenerate, OperationEmbed, OperationTranscription}
}

func (o Operation) Validate() error {
	if slices.Contains(Operations(), o) {
		return nil
	}
	return fmt.Errorf("unknown inference operation %q", o)
}
