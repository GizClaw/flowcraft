package wire

import "github.com/goccy/go-json"

// Encode marshals one message.
//
// Encoding goes through the module's JSON package rather than encoding/json so
// a message means the same thing on both sides of the wire and in the logs.
func Encode(v any) ([]byte, error) { return json.Marshal(v) }

// Decode unmarshals one message.
//
// Unknown fields are ignored, which is what lets an added field be a compatible
// change: an old client drops what it does not know instead of refusing the
// message. Required-field checking is the caller's job — this layer carries no
// policy and no schema enforcement.
func Decode(data []byte, v any) error { return json.Unmarshal(data, v) }
