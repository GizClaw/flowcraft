package memorytest

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/memory"
)

func TestValidateCompileCoverageRejectsBrokenLedgers(t *testing.T) {
	active := []memory.FieldID{memory.FieldLoadScope, memory.FieldLoadLimit}
	tests := []struct {
		name string
		res  memory.CompileResult
	}{
		{
			name: "missing active field",
			res: memory.CompileResult{Op: memory.OpLoad, Decisions: []memory.Decision{
				{Field: memory.FieldLoadScope, Disposition: memory.DispositionNative},
			}},
		},
		{
			name: "duplicate active field",
			res: memory.CompileResult{Op: memory.OpLoad, Decisions: []memory.Decision{
				{Field: memory.FieldLoadScope, Disposition: memory.DispositionNative},
				{Field: memory.FieldLoadScope, Disposition: memory.DispositionNative},
				{Field: memory.FieldLoadLimit, Disposition: memory.DispositionNative},
			}},
		},
		{
			name: "inactive field",
			res: memory.CompileResult{Op: memory.OpLoad, Decisions: []memory.Decision{
				{Field: memory.FieldLoadScope, Disposition: memory.DispositionNative},
				{Field: memory.FieldLoadLimit, Disposition: memory.DispositionNative},
				{Field: memory.FieldRecallQuery, Disposition: memory.DispositionNative},
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateCompileCoverage(test.res, active); err == nil {
				t.Fatal("broken implementation ledger was accepted")
			}
		})
	}
}
