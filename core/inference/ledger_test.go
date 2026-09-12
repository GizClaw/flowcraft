package inference

import (
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

func ledgerFields(operation Operation) []FieldID {
	return []FieldID{
		FieldGenerateInputText,
		FieldGenerateIntentTemperature,
		FieldGenerateIntentReasoningEffort,
	}
}

// TestLedgerDefaultsToNative pins the contract every driver relies on: a field
// the compiler says nothing about ends the compile as Native, in declaration
// order.
func TestLedgerDefaultsToNative(t *testing.T) {
	active := ledgerFields(OperationGenerate)
	report := NewLedger(OperationGenerate, "test", active).Report()
	if report.Operation != OperationGenerate {
		t.Fatalf("operation = %q", report.Operation)
	}
	if len(report.Decisions) != len(active) {
		t.Fatalf("decisions = %d, want %d", len(report.Decisions), len(active))
	}
	for index, decision := range report.Decisions {
		if decision.Field != active[index] || decision.Disposition != Native {
			t.Fatalf("decision %d = %+v", index, decision)
		}
	}
	if err := report.ValidateSuccess(OperationGenerate, active); err != nil {
		t.Fatalf("ValidateSuccess: %v", err)
	}
}

// TestLedgerDropKeepsCompileSuccessful pins the dropped disposition: an
// intentional discard carries a reason and does not fail the compile.
func TestLedgerDropKeepsCompileSuccessful(t *testing.T) {
	active := ledgerFields(OperationGenerate)
	ledger := NewLedger(OperationGenerate, "test", active)
	ledger.Drop(FieldGenerateIntentTemperature, "surface has no sampling knob")
	report := ledger.Report()

	if err := report.ValidateSuccess(OperationGenerate, active); err != nil {
		t.Fatalf("ValidateSuccess: %v", err)
	}
	if !report.Dropped(FieldGenerateIntentTemperature) {
		t.Fatal("report does not record the drop")
	}
	if ledger.Rejected() {
		t.Fatal("a drop must not mark the compile as rejected")
	}
	if err := ledger.Err(); err != nil {
		t.Fatalf("Err after a drop = %v, want nil", err)
	}
}

// TestLedgerRejectWinsAndCarriesTheReason pins the failure path: the first
// rejection in rejection order becomes the error field, and a drop on the same
// field is superseded.
func TestLedgerRejectWinsAndCarriesTheReason(t *testing.T) {
	active := ledgerFields(OperationGenerate)
	ledger := NewLedger(OperationGenerate, "test", active)
	ledger.Drop(FieldGenerateInputText, "not really dropped")
	ledger.Reject(FieldGenerateInputText, "model is image-only")
	ledger.Reject(FieldGenerateIntentReasoningEffort, "no reasoning channel")
	report := ledger.Report()

	if err := report.ValidateFailure(OperationGenerate, active); err != nil {
		t.Fatalf("ValidateFailure: %v", err)
	}
	err := ledger.Err()
	if !IsKind(err, UnsupportedFeature) {
		t.Fatalf("Err = %v, want %v", err, UnsupportedFeature)
	}
	var inferenceErr *Error
	if !errors.As(err, &inferenceErr) {
		t.Fatalf("Err is not an *Error: %v", err)
	}
	if inferenceErr.Field != FieldGenerateInputText {
		t.Fatalf("field = %q, want the first rejection", inferenceErr.Field)
	}
	// The provider label is diagnostic, so it lives in the cause: Error() is
	// the redacted form by contract.
	cause := errors.Unwrap(err)
	if cause == nil || !strings.Contains(cause.Error(), "test") {
		t.Fatalf("rejection cause does not carry the provider label: %v", cause)
	}
}

// TestLedgerExtensionRejectionClassifiesAsInvalidExtension pins the one
// classification the ledger owns: an extension field is an invalid extension,
// everything else an unsupported feature.
func TestLedgerExtensionRejectionClassifiesAsInvalidExtension(t *testing.T) {
	active := []FieldID{"extension.openai.generate_options.cache_key"}
	ledger := NewLedger(OperationGenerate, "test", active)
	ledger.Reject(active[0], "cache_key is unknown")
	if err := ledger.Err(); !IsKind(err, InvalidExtension) {
		t.Fatalf("Err = %v, want %v", err, InvalidExtension)
	}
}

// TestLedgerNilIsUsable pins the defensive path: a nil ledger reports nothing
// and rejects nothing instead of panicking.
func TestLedgerNilIsUsable(t *testing.T) {
	var ledger *Ledger
	ledger.Reject(FieldGenerateInputText, "reason")
	ledger.Drop(FieldGenerateInputText, "reason")
	ledger.DropComponents(FieldGenerateInputText, []ComponentNote{{Index: 0}}, "reason")
	if ledger.Rejected() {
		t.Fatal("a nil ledger cannot have rejected anything")
	}
	if err := ledger.Err(); err != nil {
		t.Fatalf("Err = %v, want nil", err)
	}
	if report := ledger.Report(); len(report.Decisions) != 0 {
		t.Fatalf("Report = %+v, want empty", report)
	}
}

// TestLedgerDropComponentsAttachesNotes pins the reason the helper exists: a
// drop of an aggregated field can explain which components reached the wire.
func TestLedgerDropComponentsAttachesNotes(t *testing.T) {
	active := ledgerFields(OperationGenerate)
	ledger := NewLedger(OperationGenerate, "test", active)
	notes := []ComponentNote{
		{Kind: message.PartText, Disposition: Native, Index: 0},
		{
			Kind:        message.PartImage,
			Disposition: Dropped,
			Index:       1,
			Reason:      "image omitted",
		},
	}
	ledger.DropComponents(FieldGenerateInputText, notes, "text-only surface")
	report := ledger.Report()

	if err := report.ValidateSuccess(OperationGenerate, active); err != nil {
		t.Fatalf("ValidateSuccess: %v", err)
	}
	got := report.Components(FieldGenerateInputText)
	if len(got) != len(notes) {
		t.Fatalf("components = %+v, want %+v", got, notes)
	}
	for index, note := range got {
		if note != notes[index] {
			t.Fatalf("component %d = %+v, want %+v", index, note, notes[index])
		}
	}
	// The report owns its notes: mutating what the caller recorded afterwards
	// must not change it.
	notes[1].Reason = "rewritten"
	if again := report.Components(FieldGenerateInputText); again[1].Reason != "image omitted" {
		t.Fatalf("report shares the caller's notes: %+v", again)
	}
}

// TestLedgerDropComponentsEmptyNotesDegradesToDrop pins the empty case: no
// components means an ordinary drop, with no decision-level noise.
func TestLedgerDropComponentsEmptyNotesDegradesToDrop(t *testing.T) {
	active := ledgerFields(OperationGenerate)
	ledger := NewLedger(OperationGenerate, "test", active)
	ledger.DropComponents(FieldGenerateIntentTemperature, nil, "no sampling knob")
	report := ledger.Report()
	if err := report.ValidateSuccess(OperationGenerate, active); err != nil {
		t.Fatalf("ValidateSuccess: %v", err)
	}
	if got := report.Components(FieldGenerateIntentTemperature); len(got) != 0 {
		t.Fatalf("components = %+v, want none", got)
	}
	if !report.Dropped(FieldGenerateIntentTemperature) {
		t.Fatal("the field was not dropped")
	}
}

// TestLedgerDropKeepsComponentNotes pins that a later plain Drop does not erase
// notes recorded earlier, in either order.
func TestLedgerDropKeepsComponentNotes(t *testing.T) {
	notes := []ComponentNote{{
		Kind:        message.PartImage,
		Disposition: Dropped,
		Index:       0,
		Reason:      "image omitted",
	}}
	for name, apply := range map[string]func(*Ledger){
		"drop then components": func(ledger *Ledger) {
			ledger.Drop(FieldGenerateInputText, "first reason")
			ledger.DropComponents(FieldGenerateInputText, notes, "second reason")
		},
		"components then drop": func(ledger *Ledger) {
			ledger.DropComponents(FieldGenerateInputText, notes, "first reason")
			ledger.Drop(FieldGenerateInputText, "second reason")
		},
	} {
		t.Run(name, func(t *testing.T) {
			active := ledgerFields(OperationGenerate)
			ledger := NewLedger(OperationGenerate, "test", active)
			apply(ledger)
			report := ledger.Report()
			if got := report.Components(FieldGenerateInputText); len(got) != 1 {
				t.Fatalf("components = %+v, want the recorded note", got)
			}
			// The first reason recorded wins, matching Drop's semantics.
			for _, decision := range report.Decisions {
				if decision.Field != FieldGenerateInputText {
					continue
				}
				if decision.Reason != "first reason" {
					t.Fatalf("reason = %q, want the first recorded", decision.Reason)
				}
			}
		})
	}
}

// TestLedgerDropComponentsNotesMustFoldOntoTheDrop pins the contract the report
// enforces: component dispositions fold to the field disposition. Recording
// all-native notes for a dropped field produces a report the runtime rejects,
// which is why the rule is documented rather than validated here.
func TestLedgerDropComponentsNotesMustFoldOntoTheDrop(t *testing.T) {
	active := ledgerFields(OperationGenerate)
	ledger := NewLedger(OperationGenerate, "test", active)
	ledger.DropComponents(FieldGenerateInputText, []ComponentNote{{
		Kind:        message.PartText,
		Disposition: Native,
		Index:       0,
	}}, "nothing was actually degraded")

	err := ledger.Report().ValidateSuccess(OperationGenerate, active)
	if err == nil {
		t.Fatal("all-native notes on a dropped field must not validate")
	}
	if !IsKind(err, CompilerContractViolation) {
		t.Fatalf("err = %v, want %v", err, CompilerContractViolation)
	}
}

// TestLedgerRejectionWinsOverComponentNotes pins that a field rejected after it
// was dropped reports the rejection and drops the notes with it: a rejected
// field never reached the wire, so its component detail is meaningless.
func TestLedgerRejectionWinsOverComponentNotes(t *testing.T) {
	active := ledgerFields(OperationGenerate)
	ledger := NewLedger(OperationGenerate, "test", active)
	ledger.DropComponents(FieldGenerateInputText, []ComponentNote{{
		Kind:        message.PartImage,
		Disposition: Dropped,
		Index:       0,
		Reason:      "image omitted",
	}}, "text-only surface")
	ledger.Reject(FieldGenerateInputText, "model is image-only")

	report := ledger.Report()
	for _, decision := range report.Decisions {
		if decision.Field != FieldGenerateInputText {
			continue
		}
		if decision.Disposition != Rejected {
			t.Fatalf("disposition = %q, want %q", decision.Disposition, Rejected)
		}
		if len(decision.Components) != 0 {
			t.Fatalf("rejection carries components: %+v", decision.Components)
		}
	}
}
