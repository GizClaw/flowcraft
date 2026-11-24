package telemetry

import "go.opentelemetry.io/otel/attribute"

// Attr is one attribute value. It is the OpenTelemetry type itself, given a name
// this package can use in its own signatures.
//
// The point is the import list at the call site. A component that logs one
// string should import this package, not the OpenTelemetry SDK: the key
// constants in attrs.go are plain strings for exactly that reason, and values are
// the other half of the same argument. A name is not a dependency boundary if
// every value beside it drags the SDK in.
//
// It is an alias rather than a wrapper on purpose: []Attr is []attribute.KeyValue,
// so a caller that already holds attribute values passes them without conversion,
// and the logging helpers keep taking the variadic list they always took.
type Attr = attribute.KeyValue

// String builds a string attribute.
func String(key, value string) Attr { return attribute.String(key, value) }

// Int builds an int attribute.
func Int(key string, value int) Attr { return attribute.Int(key, value) }

// Int64 builds an int64 attribute.
func Int64(key string, value int64) Attr { return attribute.Int64(key, value) }

// Bool builds a bool attribute.
func Bool(key string, value bool) Attr { return attribute.Bool(key, value) }
