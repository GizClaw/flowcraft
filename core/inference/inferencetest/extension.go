package inferencetest

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

// ExtensionSample is one extension value a driver declares, with the option names
// it claims to set.
type ExtensionSample struct {
	// Name labels the subtest, usually the extension ID.
	Name string
	// Value is the extension with every option it offers set. Its dynamic type
	// must be a pointer to a struct.
	Value inference.Extension
	// Fields are the option names Value sets. Nil skips the comparison.
	Fields []inference.ExtensionField
}

// RunExtensions runs the contract every extension owes the runtime, whichever
// provider it belongs to:
//
//   - an identity that is present and turns a local name into a canonical field;
//   - ActiveFields that is empty for an empty value — a declaration that sets
//     nothing must not claim to have set anything — and equal to the options a
//     populated value sets;
//   - Validate that accepts both, since a driver's own sample is a declaration it
//     promises works;
//   - Clone that owns its storage: mutating anything reachable from the copy must
//     leave the original untouched.
//
// The last one is the reason this suite exists. A clone is handed to a second
// caller, and a shared pointer or map means one request's tuning silently changes
// another's — the kind of bug that surfaces as an impossible-to-reproduce answer.
func RunExtensions(t *testing.T, samples ...ExtensionSample) {
	t.Helper()
	if len(samples) == 0 {
		t.Fatalf("inferencetest: extension suite needs at least one sample")
	}
	for _, sample := range samples {
		sample := sample
		t.Run(sample.Name, func(t *testing.T) {
			zero, ok := newZero(sample.Value)
			if !ok {
				t.Fatalf("inferencetest: extension %T is not a pointer to a struct, so its empty value cannot be derived", sample.Value)
			}
			for _, check := range []struct {
				name    string
				problem []string
			}{
				{name: "identity", problem: identityProblems(zero, sample.Value)},
				{name: "active fields", problem: activeFieldProblems(zero, sample)},
				{name: "validate", problem: validateProblems(zero, sample.Value)},
				{name: "clone", problem: cloneProblems(sample.Value)},
			} {
				t.Run(check.name, func(t *testing.T) {
					for _, problem := range check.problem {
						t.Error(problem)
					}
				})
			}
		})
	}
}

// identityProblems checks that an extension can be addressed: a value with no
// identity cannot be matched to the driver that consumes it, and a field that
// does not qualify cannot be reported as used.
func identityProblems(zero, value inference.Extension) []string {
	var problems []string
	for _, extension := range []inference.Extension{zero, value} {
		if extension.ProviderID() == "" {
			problems = append(problems, "provider id is empty")
		}
		if extension.ExtensionID() == "" {
			problems = append(problems, "extension id is empty")
		}
	}
	field := inference.ExtensionField("option").Qualify(value)
	want := "extension." + value.ProviderID() + "." + value.ExtensionID() + ".option"
	if string(field) != want {
		problems = append(problems, fmt.Sprintf("qualified field = %q, want %q", field, want))
	}
	return problems
}

// activeFieldProblems checks the report a driver makes about what it consumed. An
// empty value reports nothing; a populated one reports exactly the options it sets,
// and every name it reports has to be one it carries — a name from nowhere would
// make a dropped option undetectable.
func activeFieldProblems(zero inference.Extension, sample ExtensionSample) []string {
	if fields := zero.ActiveFields(); len(fields) != 0 {
		return []string{fmt.Sprintf("an empty value reports %v", fields)}
	}
	if sample.Fields == nil {
		return nil
	}
	var problems []string
	if got, want := sample.Value.ActiveFields(), sample.Fields; !sameSet(fieldSet(got), fieldSet(want)) {
		problems = append(problems, fmt.Sprintf("active fields = %v, want %v", got, want))
	}
	names := jsonNames(sample.Value)
	for _, field := range sample.Fields {
		if !names[string(field)] {
			problems = append(problems,
				fmt.Sprintf("active field %q is not an option the value carries", field))
		}
	}
	return problems
}

// validateProblems checks that the value is one a deployment could write: an empty
// declaration and the driver's own sample both have to be accepted.
func validateProblems(zero, value inference.Extension) []string {
	var problems []string
	if err := zero.Validate(); err != nil {
		problems = append(problems, fmt.Sprintf("an empty value was refused: %v", err))
	}
	if err := value.Validate(); err != nil {
		problems = append(problems, fmt.Sprintf("the driver's own sample was refused: %v", err))
	}
	return problems
}

// cloneProblems checks that a copy owns its storage. Everything reachable from the
// clone is overwritten, and the original has to be where it was: a shared pointer,
// slice or map would let one request's tuning reach another's.
func cloneProblems(value inference.Extension) []string {
	cloned := value.Clone()
	if cloned == nil {
		return []string{"clone returned nil"}
	}
	if reflect.TypeOf(cloned) != reflect.TypeOf(value) {
		return []string{fmt.Sprintf("clone is %T, want %T", cloned, value)}
	}
	var problems []string
	if provider, extension := cloned.ProviderID(), cloned.ExtensionID(); provider != value.ProviderID() ||
		extension != value.ExtensionID() {
		problems = append(problems, fmt.Sprintf("clone identity = %s/%s, want %s/%s",
			provider, extension, value.ProviderID(), value.ExtensionID()))
	}
	before := snapshot(value)
	clear(cloned)
	if !reflect.DeepEqual(before, snapshot(value)) {
		problems = append(problems, fmt.Sprintf(
			"clone shares storage with the original: %+v became %+v", before, snapshot(value)))
	}
	return problems
}

// newZero derives the empty value of an extension's own type, so a driver does
// not have to write "the same type with nothing set" by hand.
func newZero(value inference.Extension) (inference.Extension, bool) {
	typ := reflect.TypeOf(value)
	if typ == nil || typ.Kind() != reflect.Pointer {
		return nil, false
	}
	zero, ok := reflect.New(typ.Elem()).Interface().(inference.Extension)
	return zero, ok
}

// snapshot copies what a value points at, so a mutation through a clone is
// visible by comparison.
func snapshot(value inference.Extension) any {
	pointed := reflect.ValueOf(value)
	for pointed.Kind() == reflect.Pointer {
		if pointed.IsNil() {
			return nil
		}
		pointed = pointed.Elem()
	}
	if !pointed.IsValid() {
		return nil
	}
	copied := reflect.New(pointed.Type()).Elem()
	copied.Set(pointed)
	return deepRead(copied).Interface()
}

// deepRead rebuilds a value graph with fresh storage, so a later mutation of the
// original cannot reach the snapshot.
func deepRead(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return value
		}
		fresh := reflect.New(value.Type().Elem())
		fresh.Elem().Set(deepRead(value.Elem()))
		return fresh
	case reflect.Interface:
		if value.IsNil() {
			return value
		}
		inner := deepRead(value.Elem())
		fresh := reflect.New(value.Type()).Elem()
		fresh.Set(inner)
		return fresh
	case reflect.Slice:
		if value.IsNil() {
			return value
		}
		fresh := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := range value.Len() {
			fresh.Index(index).Set(deepRead(value.Index(index)))
		}
		return fresh
	case reflect.Map:
		if value.IsNil() {
			return value
		}
		fresh := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			fresh.SetMapIndex(deepRead(iter.Key()), deepRead(iter.Value()))
		}
		return fresh
	case reflect.Struct:
		fresh := reflect.New(value.Type()).Elem()
		for index := range value.NumField() {
			target := fresh.Field(index)
			if !target.CanSet() {
				// Unexported storage cannot be rebuilt through reflection, and a
				// driver's extensions do not hide their options.
				continue
			}
			target.Set(deepRead(value.Field(index)))
		}
		return fresh
	default:
		return value
	}
}

// clear overwrites everything reachable from a value with zero values. Applied to
// a clone, it leaves the original untouched only if the clone owns its storage.
func clear(value inference.Extension) {
	root := reflect.ValueOf(value)
	if !root.IsValid() {
		return
	}
	if root.Kind() == reflect.Pointer {
		if root.IsNil() {
			return
		}
		zeroOut(root.Elem())
		return
	}
	// An extension implemented on a value still shares whatever it points at, so
	// the copy of the header is enough to reach the storage a shallow Clone
	// handed over.
	scratch := reflect.New(root.Type()).Elem()
	scratch.Set(root)
	zeroOut(scratch)
}

func zeroOut(value reflect.Value) {
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		// Mutate through the pointer: that is the storage a shallow copy shares.
		if value.IsNil() {
			return
		}
		zeroOut(value.Elem())
	case reflect.Interface:
		if value.IsNil() {
			return
		}
		zeroOut(value.Elem())
	case reflect.Slice:
		for index := range value.Len() {
			zeroOut(value.Index(index))
		}
	case reflect.Map:
		for _, key := range value.MapKeys() {
			value.SetMapIndex(key, reflect.Zero(value.Type().Elem()))
		}
	case reflect.Struct:
		for index := range value.NumField() {
			field := value.Field(index)
			if !field.CanSet() {
				continue
			}
			zeroOut(field)
		}
	default:
		if value.CanSet() {
			value.Set(reflect.Zero(value.Type()))
		}
	}
}

// fieldSet turns a field list into a set for order-insensitive comparison.
func fieldSet(fields []inference.ExtensionField) map[inference.ExtensionField]bool {
	set := make(map[inference.ExtensionField]bool, len(fields))
	for _, field := range fields {
		set[field] = true
	}
	return set
}

func sameSet(got, want map[inference.ExtensionField]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for field := range want {
		if !got[field] {
			return false
		}
	}
	return true
}

// jsonNames is the set of keys a value serialises to, plus the goroutine-visible
// field names, so a driver that names its options differently from its json tags
// is not failed for it.
func jsonNames(value inference.Extension) map[string]bool {
	names := map[string]bool{}
	typ := reflect.TypeOf(value)
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == nil || typ.Kind() != reflect.Struct {
		return names
	}
	for index := range typ.NumField() {
		field := typ.Field(index)
		tag := field.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = field.Name
		}
		if name == "-" {
			continue
		}
		names[name] = true
		names[field.Name] = true
		names[lowerFirst(field.Name)] = true
	}
	return names
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
