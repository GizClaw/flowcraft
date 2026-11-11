package vector

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// Vector is one persisted embedding: one float32 per dimension.
//
// Its JSON encoding is this package's rather than encoding/json's, and it is a
// base64 string of the components' little-endian IEEE-754 bytes rather than a
// JSON array of numbers.
//
// A decimal rendering of a component is a rendering choice -- the shortest form
// that reads back, of which the last digit can differ between toolchains -- and
// a lane's digests are computed over the bytes of the state it stores, so a
// rendering nobody owns made the digests depend on which toolchain compiled the
// reader: Go 1.26 and Go 1.27 rounded a decimal tie in opposite directions, and
// a read then failed its digest check on state nothing had changed (#654). The
// components' bytes have no such freedom; every build writes the same
// characters for the same float32.
//
// It is also smaller and cheaper to read. The decimal form averaged 14.1 bytes
// per dimension where the base64 form costs 5.33: one 9324-entry,
// 2048-dimension table, which carries 72.8MB of vector payload, was stored as
// 270MB of text and is stored as 97MB now. Decoding is a copy of bytes instead
// of a decimal parse per component, and a materialization is what pays for it.
type Vector []float32

var _ json.Marshaler = Vector(nil)
var _ json.Unmarshaler = (*Vector)(nil)

// MarshalJSON implements json.Marshaler.
func (value Vector) MarshalJSON() ([]byte, error) {
	if value == nil {
		return []byte("null"), nil
	}
	payload := make([]byte, len(value)*4)
	for index, component := range value {
		if math.IsNaN(float64(component)) || math.IsInf(float64(component), 0) {
			// encoding/json refused these too: a component that is not a finite
			// number has no JSON, so a state that holds one is not persistable.
			return nil, errors.New("vector projection: vector component is not a finite number")
		}
		binary.LittleEndian.PutUint32(payload[index*4:], math.Float32bits(component))
	}
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(payload))+2)
	encoded[0] = '"'
	base64.StdEncoding.Encode(encoded[1:], payload)
	encoded[len(encoded)-1] = '"'
	return encoded, nil
}

// UnmarshalJSON implements json.Unmarshaler. It accepts both the base64 string
// and the array of numbers this package wrote before the payload became bytes:
// only the rendering has to be the current one, and a reader that insisted on
// its own form could not read the state a store already holds.
func (value *Vector) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		payload, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return fmt.Errorf("vector projection: decode vector payload: %w", err)
		}
		if len(payload)%4 != 0 {
			return fmt.Errorf("vector projection: vector payload of %d bytes is not whole float32 components", len(payload))
		}
		components := make([]float32, len(payload)/4)
		for index := range components {
			components[index] = math.Float32frombits(binary.LittleEndian.Uint32(payload[index*4:]))
		}
		*value = components
		return nil
	}
	var components []float32
	if err := json.Unmarshal(data, &components); err != nil {
		return err
	}
	*value = components
	return nil
}
