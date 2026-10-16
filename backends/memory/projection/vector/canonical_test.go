package vector

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"testing"

	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
)

// TestCanonicalFloat32Forms pins the rendering a component is persisted as.
// The values are the ones two toolchains disagree about, or that sit on a
// branch of the rendering (a tie, a sign, a subnormal, either end of the
// plain/scientific split, a carry through every digit).
func TestCanonicalFloat32Forms(t *testing.T) {
	cases := []struct {
		bits     uint32
		expected string
	}{
		{0x39800000, "0.00024414062"}, // 2^-12: an exact tie, 1.26 writes 0.00024414063
		{0xb9800000, "-0.00024414062"},
		{0x3f800000, "1"},
		{0xbf800000, "-1"},
		{0x00000000, "0"},
		{0x80000000, "-0"},
		{0x00000001, "1e-45"},         // the smallest subnormal
		{0x007fffff, "1.1754942e-38"}, // the largest subnormal
		{0x00800000, "1.1754944e-38"}, // the smallest normal
		{0x7f7fffff, "3.4028235e+38"},
		{0x358637bd, "0.000001"},              // the plain side of the 1e-6 split
		{0x358637bc, "9.999999e-7"},           // just below it
		{0x60ad78ec, "100000000000000000000"}, // 1e20: just below the 1e21 split
		{0x60ad78ed, "100000010000000000000"},
		{0x322bcc77, "1e-8"},
		{0x2edbe6ff, "1e-10"},
		{0x1d000000, "1.6940659e-21"},
		{0x4b189680, "10000000"},
		{0x5f000000, "9223372000000000000"},
		{0x3a83126f, "0.001"},
		{0x3dcccccd, "0.1"},
		{0x40490fdb, "3.1415927"},
		{0x3f7fffff, "0.99999994"},
		{0x421c0000, "39"}, // a trailing zero the significant digits must not keep
	}
	for _, item := range cases {
		encoded, err := appendFloat32(nil, math.Float32frombits(item.bits))
		if err != nil {
			t.Fatalf("%08x: %v", item.bits, err)
		}
		if string(encoded) != item.expected {
			t.Errorf("%08x: rendered %s, want %s", item.bits, encoded, item.expected)
		}
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

// TestVectorDigitsAreTheShortestThatReadBack checks the rendered form against
// an independent shortest-decimal search over exact rationals: for every
// length it takes the two decimals of that length that straddle the
// component, keeps the ones that read back, and stops at the first length
// that has one -- taking the closer of the two, and the even one of a pair it
// is exactly between. It shares no code with the encoder, which is the point.
func TestVectorDigitsAreTheShortestThatReadBack(t *testing.T) {
	checked := 0
	for bits := uint64(1); bits < 1<<32; bits += 524_287 { // an odd stride: no binade is skipped
		value := math.Float32frombits(uint32(bits))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			continue
		}
		encoded, err := appendFloat32(nil, value)
		if err != nil {
			t.Fatalf("%08x: %v", uint32(bits), err)
		}
		digits, exponent := parseRendered(t, string(encoded))
		wantDigits, wantExponent := referenceShortest(t, value)
		if digits != wantDigits || exponent != wantExponent {
			t.Fatalf("%08x (%v): rendered %s = %s x 10^%d, want %s x 10^%d",
				uint32(bits), value, encoded, digits, exponent, wantDigits, wantExponent)
		}
		checked++
	}
	if checked < 7000 {
		t.Fatalf("only %d components checked", checked)
	}
}

// referenceShortest finds the shortest decimal that reads back as value with
// exact rational arithmetic.
func referenceShortest(t *testing.T, value float32) (string, int) {
	t.Helper()
	magnitude := math.Abs(float64(value))
	exact := new(big.Rat).SetFloat64(magnitude)
	lead := decimalExponent(exact, magnitude)
	for count := 1; count <= 20; count++ {
		place := lead - count + 1
		step := powerOfTen(place)
		scaled := new(big.Rat).Quo(exact, step)
		below := new(big.Int).Quo(scaled.Num(), scaled.Denom())
		candidates := []*big.Int{below}
		if new(big.Rat).SetInt(below).Cmp(scaled) != 0 {
			candidates = append(candidates, new(big.Int).Add(below, big.NewInt(1)))
		}
		var reading []*big.Int
		for _, candidate := range candidates {
			if parseDecimal(t, candidate, place) == float32(magnitude) {
				reading = append(reading, candidate)
			}
		}
		switch len(reading) {
		case 0:
			continue
		case 1:
			return digitsOf(reading[0]), lead
		}
		// Both read back: the closer one, and the even one of a pair the
		// component is exactly between.
		low, high := new(big.Rat).SetInt(reading[0]), new(big.Rat).SetInt(reading[1])
		lowDistance := new(big.Rat).Sub(exact, new(big.Rat).Mul(low, step))
		highDistance := new(big.Rat).Sub(new(big.Rat).Mul(high, step), exact)
		switch lowDistance.Cmp(highDistance) {
		case -1:
			return digitsOf(reading[0]), lead
		case 1:
			return digitsOf(reading[1]), lead
		}
		if reading[0].Bit(0) == 0 {
			return digitsOf(reading[0]), lead
		}
		return digitsOf(reading[1]), lead
	}
	t.Fatalf("%v: no shortest form found", value)
	return "", 0
}

// decimalExponent returns floor(log10(value)) of a positive rational: the
// logarithm seeds the answer and the exact comparisons settle it.
func decimalExponent(value *big.Rat, magnitude float64) int {
	lead := int(math.Floor(math.Log10(magnitude)))
	for value.Cmp(powerOfTen(lead)) < 0 {
		lead--
	}
	for value.Cmp(new(big.Rat).Mul(powerOfTen(lead), big.NewRat(10, 1))) >= 0 {
		lead++
	}
	return lead
}

// parseDecimal reads the digits x 10^exponent a candidate stands for.
func parseDecimal(t *testing.T, digits *big.Int, exponent int) float32 {
	t.Helper()
	parsed, err := strconv.ParseFloat(digits.String()+"e"+strconv.Itoa(exponent), 32)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		t.Fatal(err)
	}
	return float32(parsed)
}

// digitsOf returns the digits of a scaled significand, trailing zeros removed.
func digitsOf(digits *big.Int) string {
	return strings.TrimRight(digits.String(), "0")
}

// powerOfTen returns 10^exponent as an exact rational, negative exponents
// included.
func powerOfTen(exponent int) *big.Rat {
	absolute := exponent
	if absolute < 0 {
		absolute = -absolute
	}
	value := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absolute)), nil))
	if exponent < 0 {
		return value.Inv(value)
	}
	return value
}

// parseRendered reads a rendered JSON number back into its significant digits
// and the exponent of its first digit.
func parseRendered(t *testing.T, text string) (string, int) {
	t.Helper()
	text = strings.TrimPrefix(text, "-")
	if strings.Contains(text, "e") {
		return splitExponential(text)
	}
	whole, fraction := text, ""
	if point := strings.IndexByte(text, '.'); point >= 0 {
		whole, fraction = text[:point], text[point+1:]
	}
	digits := whole + fraction
	leading := len(digits) - len(strings.TrimLeft(digits, "0"))
	return strings.TrimRight(digits[leading:], "0"), len(whole) - leading - 1
}

// TestPersistedStateKeepsVectorsCanonical walks the state the lane persists
// and fails on a float the canonical encoding does not cover: a float32 field
// that is not a Vector would be rendered by encoding/json instead, and the
// digests of the state would go back to depending on the toolchain.
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
