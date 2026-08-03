package memorytest

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/memory"
)

// CompileSuite is the black-box contract for a single op's
// compile path on a single impl. The suite drives the impl's
// CompileXxx and asserts:
//
//   - the operation name matches;
//   - every active field has exactly one decision;
//   - no decision covers a non-active field;
//   - the rejections the suite lists are reported as Rejected
//     with a Reason.
//
// A compliant impl with no unsupported features is exercised by
// passing an empty Rejections list; CompileAllNative runs the
// happy-path sub-tests in that case.
type CompileSuite struct {
	Spec         memory.Spec
	BuildRuntime func(t *testing.T) *memory.Runtime

	// Operation is the op the suite tests.
	Operation memory.Operation

	// NewRequest returns a fresh, valid request. Each sub-test
	// mutates one field to drive the rejection cases.
	NewRequest func() any

	// ActiveFields enumerates the canonical fields of this op's
	// request. The kernel's own xxxActiveFields is the
	// authoritative source.
	ActiveFields func(req any) []memory.FieldID

	// Compile runs the impl's CompileXxx for the given op. The
	// body should switch on Operation and call the matching
	// AppendOp.CompileAppend / LoadOp.CompileLoad / … method on
	// the runtime's resolved impl.
	Compile func(rt *memory.Runtime, ctx context.Context, req any) memory.CompileResult

	// Rejections lists fields the impl is expected to refuse
	// when NewRequest returns a request that exercises them.
	// An empty list means "this impl supports everything".
	Rejections []CompileRejection
}

// CompileRejection names one canonical field an impl refuses
// and the request that exposes the refusal.
type CompileRejection struct {
	// Name is the test sub-test name.
	Name string
	// Mutate takes a fresh request from NewRequest and
	// mutates it to expose the unsupported field.
	Mutate func(req any)
	// Field is the canonical FieldID the compile must
	// Reject.
	Field memory.FieldID
	// Kind is the Reason the impl must report. Pass an empty
	// string to accept any non-empty Reason.
	Kind memory.Reason
}

// RunCompile drives a single op's compile contract.
func RunCompile(t *testing.T, s CompileSuite) {
	t.Helper()
	if s.BuildRuntime == nil || s.NewRequest == nil ||
		s.ActiveFields == nil || s.Compile == nil {
		t.Fatal("CompileSuite requires BuildRuntime, NewRequest, ActiveFields, Compile")
	}
	if s.Operation == "" {
		t.Fatal("CompileSuite.Operation is required")
	}

	t.Run("all_native_for_valid_request", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		res := s.Compile(rt, context.Background(), s.NewRequest())
		if res.Op != s.Operation {
			t.Errorf("Op = %q, want %q", res.Op, s.Operation)
		}
		if !res.AllNative() {
			t.Errorf("expected all-Native, got: %+v", res)
		}
	})

	t.Run("ledger_covers_every_active_field", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		req := s.NewRequest()
		active := s.ActiveFields(req)
		res := s.Compile(rt, context.Background(), req)
		if err := validateCompileCoverage(res, active); err != nil {
			t.Error(err)
		}
	})

	for _, rej := range s.Rejections {
		t.Run("rejects_"+rej.Name, func(t *testing.T) {
			rt := s.BuildRuntime(t)
			req := s.NewRequest()
			if rej.Mutate != nil {
				rej.Mutate(req)
			}
			res := s.Compile(rt, context.Background(), req)
			found := false
			for _, d := range res.Decisions {
				if d.Field == rej.Field && d.Disposition == memory.DispositionRejected {
					if d.Reason == "" {
						t.Errorf("Rejected decision on %q has empty Reason", rej.Field)
					}
					if rej.Kind != "" && d.Reason != rej.Kind {
						t.Errorf("Reason = %q, want %q", d.Reason, rej.Kind)
					}
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected Rejected decision on %q, got: %+v", rej.Field, res)
			}
		})
	}
}

func validateCompileCoverage(res memory.CompileResult, active []memory.FieldID) error {
	activeCounts := make(map[memory.FieldID]int, len(active))
	for _, field := range active {
		activeCounts[field]++
		if activeCounts[field] != 1 {
			return fmt.Errorf("active field %q is listed more than once", field)
		}
	}
	decisionCounts := make(map[memory.FieldID]int, len(res.Decisions))
	for _, decision := range res.Decisions {
		if activeCounts[decision.Field] == 0 {
			return fmt.Errorf(
				"decision covers inactive field %q (active: %v, decision: %+v)",
				decision.Field, active, decision)
		}
		decisionCounts[decision.Field]++
	}
	for _, field := range active {
		if decisionCounts[field] != 1 {
			return fmt.Errorf(
				"field %q has %d decisions, want exactly 1 (active: %v, got: %+v)",
				field, decisionCounts[field], active, res)
		}
	}
	return nil
}
