package inference

import "context"

// RequestMetadata is opaque correlation data attached to one call: a
// conversation id, a turn id, a tenant id. Core never interprets the keys — they
// are defined by the deployment — so it is not a request field: a field on
// [GenerateRequest] would claim a schema this package does not own.
//
// It travels in the context because that is what it is: cross-cutting data that
// every layer in the call chain may read (telemetry attaches it to spans, a
// driver may map it onto a provider's own user/tracking parameter) and that no
// layer owns. It also survives retries and streaming without being copied
// through every layer.
type RequestMetadata map[string]string

type requestMetadataKey struct{}

// WithRequestMetadata returns a context carrying metadata. Passing nil or an
// empty map returns the context unchanged.
func WithRequestMetadata(ctx context.Context, metadata RequestMetadata) context.Context {
	if len(metadata) == 0 {
		return ctx
	}
	return context.WithValue(ctx, requestMetadataKey{}, metadata.Clone())
}

// RequestMetadataFrom returns the metadata attached to ctx, or nil. The returned
// map is owned by the context: read it, do not mutate it.
func RequestMetadataFrom(ctx context.Context) RequestMetadata {
	metadata, _ := ctx.Value(requestMetadataKey{}).(RequestMetadata)
	return metadata
}

// Get returns the value for key, or the empty string.
func (m RequestMetadata) Get(key string) string {
	if m == nil {
		return ""
	}
	return m[key]
}

// Clone returns an independent copy.
func (m RequestMetadata) Clone() RequestMetadata {
	if m == nil {
		return nil
	}
	clone := make(RequestMetadata, len(m))
	for key, value := range m {
		clone[key] = value
	}
	return clone
}
