package vector

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
)

// TestVectorPayloadIsTheComponentsBytes pins the persisted form: base64 of the
// components' little-endian IEEE-754 bytes, and nothing else. The bytes are what
// a lane's digests are computed over, so the encoding has to be a function of the
// components alone -- a build that rendered them differently would make every
// stored state fail its own digest check (#654), and a build that stored them in
// another order would read a vector as its own reflection.
func TestVectorPayloadIsTheComponentsBytes(t *testing.T) {
	cases := []struct {
		value Vector
		want  string
	}{
		{Vector{}, `""`},
		{Vector{1}, `"AACAPw=="`},
		{Vector{1, 0}, `"AACAPwAAAAA="`},
		// The sign of a zero is a component, not a rendering: -0 keeps its bit.
		{Vector{1, float32(math.Copysign(0, -1))}, `"AACAPwAAAIA="`},
		{Vector{3.1415927, -1, math.SmallestNonzeroFloat32}, `"2w9JQAAAgL8BAAAA"`},
		{Vector{1, 2, 3}, `"AACAPwAAAEAAAEBA"`},
	}
	for _, item := range cases {
		encoded, err := json.Marshal(item.value)
		if err != nil {
			t.Fatalf("%v: %v", item.value, err)
		}
		if string(encoded) != item.want {
			t.Errorf("%v: encoded %s, want %s", item.value, encoded, item.want)
		}
	}
}

// TestVectorPayloadCostsFiveBytesPerDimension is the reason the form changed,
// measured rather than assumed: the decimal rendering this package used to write
// averaged 14.1 bytes per dimension, and the eval's 9324-entry,
// 2048-dimension table spent 270MB of stored text on 72.8MB of vectors.
func TestVectorPayloadCostsFiveBytesPerDimension(t *testing.T) {
	value := make(Vector, 2048)
	for index := range value {
		value[index] = float32(index) / 7
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	perDimension := float64(len(encoded)) / float64(len(value))
	if perDimension > 5.4 {
		t.Fatalf("one 2048-dimension vector encodes to %d bytes, %.2f per dimension", len(encoded), perDimension)
	}
	if want := 2 + base64.StdEncoding.EncodedLen(4*len(value)); len(encoded) != want {
		t.Fatalf("encoded %d bytes, want %d", len(encoded), want)
	}
}

// TestVectorRejectsNonFiniteComponents pins the other half of the contract:
// a component that is not a finite number has no JSON, exactly as
// encoding/json says, and the error has to reach the caller rather than
// produce a state that cannot be read back.
func TestVectorRejectsNonFiniteComponents(t *testing.T) {
	for _, value := range []float32{float32(math.Inf(1)), float32(math.Inf(-1)), float32(math.NaN())} {
		if _, err := json.Marshal(Vector{value}); err == nil {
			t.Fatalf("%v: encoded without an error", value)
		}
	}
}

func TestVectorJSONRoundTripsComponents(t *testing.T) {
	var values []float32
	for bits := uint32(0); bits < 1<<16; bits++ {
		values = append(values, math.Float32frombits(bits))
	}
	values = append(values, math.Float32frombits(0x39800000), math.Float32frombits(0xb9800000),
		math.Float32frombits(0x7f7fffff), math.Float32frombits(0x80000000))
	data, err := json.Marshal(Vector(values))
	if err != nil {
		t.Fatal(err)
	}
	var decoded Vector
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(values) {
		t.Fatalf("decoded %d components, want %d", len(decoded), len(values))
	}
	for index, value := range values {
		if math.Float32bits(decoded[index]) != math.Float32bits(value) {
			t.Fatalf("component %d: decoded %v (%08x), want %v (%08x)",
				index, decoded[index], math.Float32bits(decoded[index]), value, math.Float32bits(value))
		}
	}
}

// TestVectorReadsTheDecimalForm pins the reader's other half: state an earlier
// build published is an array of numbers, and a reader that could not read it
// could not answer for a workspace it did not write.
func TestVectorReadsTheDecimalForm(t *testing.T) {
	var decoded Vector
	if err := json.Unmarshal([]byte(`[1,0.00024414062,-0,1e-45]`), &decoded); err != nil {
		t.Fatal(err)
	}
	want := Vector{1, math.Float32frombits(0x39800000), float32(math.Copysign(0, -1)), math.SmallestNonzeroFloat32}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded %v, want %v", decoded, want)
	}
	if err := json.Unmarshal([]byte(`null`), &decoded); err != nil || decoded != nil {
		t.Fatalf("null decoded to %v, %v", decoded, err)
	}
	if err := json.Unmarshal([]byte(`"not base64!"`), &decoded); err == nil {
		t.Fatal("accepted a payload that is not base64")
	}
	if err := json.Unmarshal([]byte(`"AAAAAAA"`), &decoded); err == nil {
		t.Fatal("accepted a payload that is not whole components")
	}
}

// TestPersistedStateKeepsVectorsCanonical walks the state the lane persists and
// fails on a float it renders itself: a float32 field that is not a Vector would
// be written by encoding/json, whose form this package does not own and cannot
// promise to keep still across toolchains.
func TestPersistedStateKeepsVectorsCanonical(t *testing.T) {
	var delta projectionstore.EntryDelta[entry]
	for _, value := range []any{snapshot{}, entry{}, delta} {
		checkCanonicalFloats(t, reflect.TypeOf(value), map[reflect.Type]bool{})
	}
}

func checkCanonicalFloats(t *testing.T, value reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if seen[value] {
		return
	}
	seen[value] = true
	switch value.Kind() {
	case reflect.Slice, reflect.Array, reflect.Pointer:
		checkCanonicalFloats(t, value.Elem(), seen)
	case reflect.Map:
		checkCanonicalFloats(t, value.Key(), seen)
		checkCanonicalFloats(t, value.Elem(), seen)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			if !field.IsExported() || strings.Split(field.Tag.Get("json"), ",")[0] == "-" {
				continue
			}
			if field.Type == reflect.TypeOf(Vector(nil)) {
				continue
			}
			switch field.Type.Kind() {
			case reflect.Float32, reflect.Float64:
				t.Errorf("%s.%s is a %s: persisted floats must be a canonical Vector",
					value, field.Name, field.Type)
			default:
				checkCanonicalFloats(t, field.Type, seen)
			}
		}
	}
}
