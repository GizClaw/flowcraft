package inferencetest

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

// Options is a minimal extension: two pointers, a slice and a map, with the
// ActiveFields a driver would report for them. Its fields are exported because the
// suite rebuilds a value through reflection.
type Options struct {
	Speed   *float64       `json:"speed,omitempty"`
	Voice   *string        `json:"voice,omitempty"`
	Tags    []string       `json:"tags,omitempty"`
	Weights map[string]int `json:"weights,omitempty"`
}

func (o Options) ProviderID() string  { return "test" }
func (o Options) ExtensionID() string { return "options" }
func (o Options) Validate() error     { return nil }

func (o Options) Clone() inference.Extension {
	o.Speed = clonePtr(o.Speed)
	o.Voice = clonePtr(o.Voice)
	o.Tags = append([]string(nil), o.Tags...)
	if o.Weights != nil {
		weights := make(map[string]int, len(o.Weights))
		for key, value := range o.Weights {
			weights[key] = value
		}
		o.Weights = weights
	}
	return o
}

func (o Options) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.Speed != nil {
		fields = append(fields, "speed")
	}
	if o.Voice != nil {
		fields = append(fields, "voice")
	}
	if len(o.Tags) > 0 {
		fields = append(fields, "tags")
	}
	if len(o.Weights) > 0 {
		fields = append(fields, "weights")
	}
	return fields
}

// Shallow is the bug the suite exists for: the pointers and the map are shared
// with the original.
type Shallow struct{ Options }

func (s Shallow) Clone() inference.Extension { return Shallow{s.Options} }

// Forgetful reports no options at all, so a driver that misses one is caught by
// the suite rather than by a wrong answer later.
type Forgetful struct{ Options }

func (f Forgetful) ActiveFields() []inference.ExtensionField { return nil }

func clonePtr[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func populated() Options {
	speed, voice := 1.25, "Cherry"
	return Options{
		Speed:   &speed,
		Voice:   &voice,
		Tags:    []string{"a"},
		Weights: map[string]int{"flow": 50},
	}
}

func allFields() []inference.ExtensionField {
	return []inference.ExtensionField{"speed", "voice", "tags", "weights"}
}

// A conforming extension has to pass: a suite that fails everything tells a
// driver nothing.
func TestRunExtensionsAcceptsAConformingExtension(t *testing.T) {
	value := populated()
	checks := map[string][]string{
		"clone":         cloneProblems(value),
		"active fields": activeFieldProblems(Options{}, ExtensionSample{Value: value, Fields: allFields()}),
		"validate":      validateProblems(Options{}, value),
		"identity":      identityProblems(Options{}, value),
	}
	for name, problems := range checks {
		if len(problems) != 0 {
			t.Errorf("%s: %v", name, problems)
		}
	}
}

// And it has to fail the two shapes a driver actually gets wrong.
func TestRunExtensionsCatchesShallowCopies(t *testing.T) {
	problems := cloneProblems(Shallow{populated()})
	if len(problems) == 0 {
		t.Fatal("a clone that shares storage was accepted")
	}
	if !strings.Contains(problems[0], "shares storage") {
		t.Errorf("problem = %q, want it to name the shared storage", problems[0])
	}
}

func TestRunExtensionsCatchesAnIncompleteFieldReport(t *testing.T) {
	problems := activeFieldProblems(Options{}, ExtensionSample{
		Value: Forgetful{populated()}, Fields: allFields(),
	})
	if len(problems) == 0 {
		t.Fatal("a value that sets four options and reports none was accepted")
	}
	if !strings.Contains(problems[0], "active fields") {
		t.Errorf("problem = %q, want it to name the report", problems[0])
	}
}

func TestRunExtensionsCatchesAFieldFromNowhere(t *testing.T) {
	problems := activeFieldProblems(Options{}, ExtensionSample{
		Value:  Options{},
		Fields: []inference.ExtensionField{"nonexistent"},
	})
	// Two complaints are expected — the value sets nothing while the sample says
	// it sets four, and the one field is an option the type does not carry — so
	// the assertion looks for the specific one.
	var named bool
	for _, problem := range problems {
		if strings.Contains(problem, "not an option the value carries") {
			named = true
		}
	}
	if !named {
		t.Fatalf("problems = %v, want the unknown option named", problems)
	}
}

// The snapshot has to be a real copy: a shallow one would compare equal after the
// original was mutated and the suite would pass everything.
func TestSnapshotOwnsItsStorage(t *testing.T) {
	value := populated()
	before := snapshot(value)
	*value.Speed = 9.0
	value.Tags[0] = "b"
	value.Weights["flow"] = 1
	kept, ok := before.(Options)
	if !ok {
		t.Fatalf("snapshot = %T, want Options", before)
	}
	if *kept.Speed != 1.25 || kept.Tags[0] != "a" || kept.Weights["flow"] != 50 {
		t.Fatalf("snapshot followed the original: %+v", kept)
	}
}
