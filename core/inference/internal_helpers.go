package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// decodeStrict decodes a JSON object/value with strict-mode settings: it
// rejects unknown fields and trailing values. inference and message
// each carry their own copy because the helper is small and both packages
// want to keep the call site local to the type it is decoding.
func decodeStrict(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}
