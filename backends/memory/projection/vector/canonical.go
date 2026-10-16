package vector

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Vector is one persisted embedding: one float32 per dimension.
//
// Its JSON encoding is this package's rather than encoding/json's, because
// encoding/json renders a float32 with the float32 bit size and that
// rendering changed between toolchains: Go 1.27 rounds a decimal tie (the
// exact value that sits half way between the two shortest candidates) to
// even, Go 1.26 rounds it away from zero. The bytes of a stored state are
// therefore fine and its digest still differs, depending on which toolchain
// compiled the binary that recomputes the encoding -- a lane's read then
// fails its digest check on state nothing has changed (#654).
//
// The canonical form is the one Go 1.27 emits: the shortest decimal that
// reads back as the same float32, ties to even, with the same plain/scientific
// split encoding/json uses. State written by a Go 1.27 build keeps verifying
// unchanged; state written by an older build still has to be re-derived, which
// is the same condition it was already in.
type Vector []float32

var _ json.Marshaler = Vector(nil)
var _ json.Unmarshaler = (*Vector)(nil)

// MarshalJSON implements json.Marshaler.
func (value Vector) MarshalJSON() ([]byte, error) {
	if value == nil {
		return []byte("null"), nil
	}
	encoded := make([]byte, 0, 2+len(value)*8)
	encoded = append(encoded, '[')
	for index, component := range value {
		if index > 0 {
			encoded = append(encoded, ',')
		}
		var err error
		if encoded, err = appendFloat32(encoded, component); err != nil {
			return nil, err
		}
	}
	return append(encoded, ']'), nil
}

// UnmarshalJSON implements json.Unmarshaler. It accepts any decimal form a
// reader before this change could have written; only the rendering has to be
// canonical, and a reader that insisted on its own form could not read the
// state a store already holds.
func (value *Vector) UnmarshalJSON(data []byte) error {
	var components []float32
	if err := json.Unmarshal(data, &components); err != nil {
		return err
	}
	*value = components
	return nil
}

// appendFloat32 appends the canonical JSON rendering of one component.
func appendFloat32(dst []byte, component float32) ([]byte, error) {
	if math.IsNaN(float64(component)) || math.IsInf(float64(component), 0) {
		// encoding/json refuses these too: a component that is not a finite
		// number has no JSON, so a state that holds one is not persistable.
		return nil, errors.New("vector projection: vector component is not a finite number")
	}
	if component == 0 {
		if math.Signbit(float64(component)) {
			return append(dst, "-0"...), nil
		}
		return append(dst, '0'), nil
	}
	negative := component < 0
	magnitude := float32(math.Abs(float64(component)))
	digits, exponent := shortestDigits(float64(magnitude))
	exact := false
	for {
		form, formExponent, decided := canonicalForm(digits, exponent, magnitude, exact)
		if decided {
			if form == "" {
				return nil, errors.New("vector projection: vector component has no canonical form")
			}
			return emitFloat32(dst, form, formExponent, magnitude, negative)
		}
		// The digits at hand show a half at some length, and they are a
		// rounding of the expansion rather than the expansion: the two differ
		// exactly in what that half is. Every float32 expands to a terminating
		// decimal, so asking it is possible.
		digits, exponent = exactDigits(magnitude)
		exact = true
	}
}

// canonicalForm picks the form of magnitude from the digits at hand: the
// shortest decimal that reads back as magnitude, and of that length the one
// closest to it (an exact half taken to even). It reports decided false when
// the digits show a half only the exact expansion can confirm.
func canonicalForm(digits string, exponent int, magnitude float32, exact bool) (string, int, bool) {
	count := shortestDigitsCount(magnitude)
	if count > len(digits) {
		count = len(digits)
	}
	// The count is where the search starts, not what it trusts: the walk
	// settles the shortest length that reads back from the digits themselves.
	form, formExponent := "", 0
	for count <= len(digits) {
		chosen, chosenExponent, reads, decided := choosePair(digits, exponent, magnitude, count, exact)
		if !decided {
			return "", 0, false
		}
		if reads {
			form, formExponent = chosen, chosenExponent
			break
		}
		count++
	}
	if form == "" {
		return "", 0, true
	}
	for count > 1 {
		chosen, chosenExponent, reads, decided := choosePair(digits, exponent, magnitude, count-1, exact)
		if !decided {
			return "", 0, false
		}
		if !reads {
			break
		}
		form, formExponent, count = chosen, chosenExponent, count-1
	}
	return form, formExponent, true
}

// choosePair compares the two decimals of the given length that straddle the
// component. The form has to be one of them: a decimal further from the
// component than both is not the closest form of it, and one between them
// would read back at this length (the decimals that read back are an interval
// that holds the component, so it holds everything between them too).
func choosePair(
	digits string,
	exponent int,
	magnitude float32,
	count int,
	exact bool,
) (string, int, bool, bool) {
	below := digits[:count]
	above, bump := roundUp(below)
	belowReads := readsBack(below, exponent, magnitude)
	aboveReads := readsBack(above, exponent+bump, magnitude)
	switch {
	case !belowReads && !aboveReads:
		return "", 0, false, true
	case belowReads && !aboveReads:
		return below, exponent, true, true
	case aboveReads && !belowReads:
		return above, exponent + bump, true, true
	}
	// Both read back, so the closer one is the form. The digits at hand say
	// which that is unless the component is exactly between them -- or unless
	// they run out, which leaves the rest of the expansion unknown.
	dropped := digits[count:]
	if dropped != "" {
		if dropped[0] > '5' || (dropped[0] == '5' && strings.Trim(dropped[1:], "0") != "") {
			return above, exponent + bump, true, true
		}
		if dropped[0] < '5' {
			return below, exponent, true, true
		}
	}
	if !exact {
		return "", 0, false, false
	}
	if dropped == "" || (below[len(below)-1]-'0')%2 == 0 {
		return below, exponent, true, true
	}
	return above, exponent + bump, true, true
}

// emitFloat32 appends the rendered form of digits x 10^exponent.
func emitFloat32(dst []byte, digits string, exponent int, magnitude float32, negative bool) ([]byte, error) {
	if negative {
		dst = append(dst, '-')
	}
	return append(dst, renderFloat32(digits, exponent, magnitude)...), nil
}

// readsBack reports whether the decimal digits x 10^exponent, written out in
// full, is a reading of value rather than of a neighbouring float32.
func readsBack(digits string, exponent int, value float32) bool {
	text := digits
	if len(digits) > 1 {
		text = digits[:1] + "." + digits[1:]
	}
	parsed, err := strconv.ParseFloat(text+"e"+strconv.Itoa(exponent), 32)
	if err != nil {
		return false
	}
	return float32(parsed) == value
}

// renderFloat32 writes digits x 10^exponent the way encoding/json splits a
// number: plain notation for a magnitude at least 1e-6 and below 1e21,
// scientific notation outside that, with the exponent sign kept and the
// exponent unpadded.
func renderFloat32(digits string, exponent int, magnitude float32) string {
	if magnitude < 1e-6 || magnitude >= 1e21 {
		sign := "+"
		if exponent < 0 {
			sign, exponent = "-", -exponent
		}
		text := digits[:1]
		if len(digits) > 1 {
			text += "." + digits[1:]
		}
		return text + "e" + sign + strconv.Itoa(exponent)
	}
	// The plain form places the point after exponent+1 digits, padding with
	// zeros on whichever side the digits do not reach.
	point := exponent + 1
	switch {
	case point <= 0:
		return "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		return digits + strings.Repeat("0", point-len(digits))
	default:
		return digits[:point] + "." + digits[point:]
	}
}

// shortestDigits returns the significant digits of magnitude and the decimal
// exponent of its first digit (value = digits[0].digits[1:] x 10^exponent).
//
// The digits come from float64's shortest rendering, which is the exact
// expansion whenever the expansion has 17 or fewer significant digits -- and
// a tie that can decide the float32 rendering always does, because the half
// way point between two decimals of at most nine digits is such a decimal.
// Where the digits are a rounding of a longer expansion, the caller detects
// the tie they cannot decide and asks exactDigits instead.
func shortestDigits(magnitude float64) (string, int) {
	return splitExponential(string(strconv.AppendFloat(nil, magnitude, 'e', -1, 64)))
}

// shortestDigitsCount is how many significant digits the shortest rendering
// of magnitude needs. The length is what "shortest" is defined by, so it is
// the same in every toolchain even where the last digit is not; the search in
// canonicalForm walks from it rather than trusting it.
func shortestDigitsCount(magnitude float32) int {
	text := string(strconv.AppendFloat(nil, float64(magnitude), 'e', -1, 32))
	if index := strings.IndexByte(text, 'e'); index >= 0 {
		text = text[:index]
	}
	return len(strings.TrimRight(strings.Replace(text, ".", "", 1), "0"))
}

// splitExponential splits "d.dddde±dd" into its digits and the exponent of
// its first digit.
func splitExponential(text string) (string, int) {
	exponent := 0
	if index := strings.IndexByte(text, 'e'); index >= 0 {
		exponent, _ = strconv.Atoi(text[index+1:])
		text = text[:index]
	}
	digits := strings.Replace(text, ".", "", 1)
	trimmed := strings.TrimRight(digits, "0")
	return trimmed, exponent
}

// exactDigits returns the exact decimal expansion of magnitude. Every float32
// is a dyadic rational, so the expansion terminates: the expansion is the
// significand times a power of five, moved by the exponent of two.
func exactDigits(magnitude float32) (string, int) {
	bits := math.Float32bits(magnitude)
	significand := uint64(bits & 0x7fffff)
	exponent := int(bits>>23&0xff) - 150
	if exponent == -150 {
		// A subnormal has no implicit leading one: value = significand x 2^-149.
		exponent = -149
	} else {
		significand |= 1 << 23
	}
	value := new(big.Int).SetUint64(significand)
	scale := 0
	if exponent >= 0 {
		value.Lsh(value, uint(exponent))
	} else {
		scale = -exponent
		value.Mul(value, new(big.Int).Exp(big.NewInt(5), big.NewInt(int64(scale)), nil))
	}
	digits := value.String()
	// value = digits x 10^-scale = 0.digits x 10^(len(digits)-scale).
	exponent = len(digits) - 1 - scale
	// Trimming trailing zeros leaves the value and the place of the first
	// digit alone, so the exponent is the one just computed.
	return strings.TrimRight(digits, "0"), exponent
}

// roundUp adds one in the last place of digits, dropping the trailing zeros
// the carry produces and reporting how far the exponent moves.
func roundUp(digits string) (string, int) {
	out := []byte(digits)
	for index := len(out) - 1; index >= 0; index-- {
		if out[index] != '9' {
			out[index]++
			return strings.TrimRight(string(out), "0"), 0
		}
		out[index] = '0'
	}
	return "1", 1
}
