package inference

import (
	"errors"
	"strings"
	"testing"
)

// dispatchGenerateOptions and dispatchImageOptions stand in for the two
// extension shapes a provider publishes: one the operation consumes, one it
// must reject.
type dispatchGenerateOptions struct {
	Level string
}

func (dispatchGenerateOptions) ProviderID() string  { return "test" }
func (dispatchGenerateOptions) ExtensionID() string { return "generate_options" }
func (o dispatchGenerateOptions) ActiveFields() []ExtensionField {
	if o.Level == "" {
		return nil
	}
	return []ExtensionField{"level"}
}
func (dispatchGenerateOptions) Validate() error { return nil }
func (o dispatchGenerateOptions) Clone() Extension {
	return o
}

type dispatchImageOptions struct {
	Mask bool
}

func (dispatchImageOptions) ProviderID() string  { return "test" }
func (dispatchImageOptions) ExtensionID() string { return "image_options" }
func (o dispatchImageOptions) ActiveFields() []ExtensionField {
	if !o.Mask {
		return nil
	}
	return []ExtensionField{"mask"}
}
func (dispatchImageOptions) Validate() error { return nil }
func (o dispatchImageOptions) Clone() Extension {
	return o
}

// dispatchPointerOptions exists to pin the typed-nil case: a nil pointer behind
// the interface is neither a match nor something to reject.
type dispatchPointerOptions struct{}

func (*dispatchPointerOptions) ProviderID() string             { return "test" }
func (*dispatchPointerOptions) ExtensionID() string            { return "pointer_options" }
func (*dispatchPointerOptions) ActiveFields() []ExtensionField { return nil }
func (*dispatchPointerOptions) Validate() error                { return nil }
func (o *dispatchPointerOptions) Clone() Extension             { return o }

func TestExtensionForSplitsByConcreteType(t *testing.T) {
	t.Run("match and foreign", func(t *testing.T) {
		matching := dispatchGenerateOptions{Level: "high"}
		foreign := dispatchImageOptions{Mask: true}
		options, other := ExtensionFor[dispatchGenerateOptions](
			Extensions{foreign, matching},
		)
		if options != matching {
			t.Fatalf("options = %+v, want %+v", options, matching)
		}
		if len(other) != 1 || other[0] != Extension(foreign) {
			t.Fatalf("other = %#v, want the foreign extension", other)
		}
	})

	t.Run("last match wins", func(t *testing.T) {
		options, other := ExtensionFor[dispatchGenerateOptions](Extensions{
			dispatchGenerateOptions{Level: "low"},
			dispatchGenerateOptions{Level: "high"},
		})
		if options.Level != "high" {
			t.Fatalf("options = %+v, want the last match", options)
		}
		if len(other) != 0 {
			t.Fatalf("other = %#v, want empty", other)
		}
	})

	t.Run("no match keeps everything", func(t *testing.T) {
		foreign := dispatchImageOptions{Mask: true}
		options, other := ExtensionFor[dispatchGenerateOptions](Extensions{foreign})
		if options != (dispatchGenerateOptions{}) {
			t.Fatalf("options = %+v, want the zero value", options)
		}
		if len(other) != 1 || other[0] != Extension(foreign) {
			t.Fatalf("other = %#v, want the foreign extension", other)
		}
	})

	t.Run("nil entries belong to neither side", func(t *testing.T) {
		matching := dispatchGenerateOptions{Level: "high"}
		var typedNil *dispatchPointerOptions
		options, other := ExtensionFor[dispatchGenerateOptions](
			Extensions{nil, typedNil, matching},
		)
		if options != matching {
			t.Fatalf("options = %+v, want %+v", options, matching)
		}
		if len(other) != 0 {
			t.Fatalf("other = %#v, want empty", other)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		options, other := ExtensionFor[dispatchGenerateOptions](nil)
		if options != (dispatchGenerateOptions{}) || len(other) != 0 {
			t.Fatalf("options = %+v, other = %#v, want zero values", options, other)
		}
	})
}

func TestLedgerRejectExtensions(t *testing.T) {
	foreign := dispatchImageOptions{Mask: true}
	field := ExtensionField("mask").Qualify(foreign)
	if field != "extension.test.image_options.mask" {
		t.Fatalf("qualified field = %q", field)
	}

	active := []FieldID{FieldGenerateInputText, field}
	ledger := NewLedger(OperationGenerate, "test", active)
	ledger.RejectExtensions("generate", Extensions{foreign})

	report := ledger.Report()
	if err := report.ValidateFailure(OperationGenerate, active); err != nil {
		t.Fatalf("ValidateFailure: %v", err)
	}
	reason := ""
	for _, decision := range report.Decisions {
		if decision.Field == field {
			if decision.Disposition != Rejected {
				t.Fatalf("disposition = %q, want rejected", decision.Disposition)
			}
			reason = decision.Reason
		}
	}
	if reason != `extension "image_options" does not apply to generate` {
		t.Fatalf("reason = %q", reason)
	}

	if !ledger.Rejected() {
		t.Fatal("ledger does not report the rejection")
	}
	var inferenceErr *Error
	if err := ledger.Err(); !errors.As(err, &inferenceErr) {
		t.Fatalf("Err = %v, want an *Error", err)
	} else {
		// A rejection of a provider extension is an invalid extension, not an
		// unsupported feature: the provider is fine, the configuration that
		// addressed this operation is not. Ledger derives that from the
		// qualified "extension." prefix of the field.
		if inferenceErr.Kind != InvalidExtension {
			t.Fatalf("kind = %q, want invalid extension", inferenceErr.Kind)
		}
		if inferenceErr.Field != field {
			t.Fatalf("field = %q, want %q", inferenceErr.Field, field)
		}
	}
}

// TestLedgerRejectExtensionsRecordsNothingForIdleExtensions pins the no-op
// path: an extension that is active in the request but carries no knob must not
// fabricate a rejection, and the compile stays successful.
func TestLedgerRejectExtensionsRecordsNothingForIdleExtensions(t *testing.T) {
	idle := dispatchImageOptions{}
	active := []FieldID{FieldGenerateInputText}
	ledger := NewLedger(OperationGenerate, "test", active)
	ledger.RejectExtensions("generate", Extensions{idle, nil})

	report := ledger.Report()
	if err := report.ValidateSuccess(OperationGenerate, active); err != nil {
		t.Fatalf("ValidateSuccess: %v", err)
	}
	if ledger.Rejected() {
		t.Fatal("an extension without active fields must not reject anything")
	}
	if err := ledger.Err(); err != nil {
		t.Fatalf("Err = %v, want nil", err)
	}
}

func TestLedgerRejectExtensionsIsNilSafe(t *testing.T) {
	var ledger *Ledger
	ledger.RejectExtensions("generate", Extensions{dispatchImageOptions{Mask: true}})
	// A typed nil behind the interface must not panic either.
	var typedNil *dispatchPointerOptions
	NewLedger(OperationGenerate, "test", nil).
		RejectExtensions("generate", Extensions{typedNil})
}

// TestExtensionSplitThenReject pins the two halves together, the way a driver
// uses them: take the operation's own options, reject what is left.
func TestExtensionSplitThenReject(t *testing.T) {
	options, other := ExtensionFor[dispatchGenerateOptions](Extensions{
		dispatchImageOptions{Mask: true},
		dispatchGenerateOptions{Level: "high"},
	})
	if options.Level != "high" {
		t.Fatalf("options = %+v", options)
	}

	field := ExtensionField("mask").Qualify(dispatchImageOptions{})
	active := []FieldID{field}
	ledger := NewLedger(OperationGenerate, "test", active)
	ledger.RejectExtensions("generate", other)

	reason := ""
	for _, decision := range ledger.Report().Decisions {
		if decision.Field == field {
			reason = decision.Reason
		}
	}
	if !strings.Contains(reason, "does not apply to generate") {
		t.Fatalf("reason = %q", reason)
	}
}
