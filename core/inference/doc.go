// Package inference defines the provider contract: the canonical shape of a
// generation call, the interface a provider implements, and the error and
// accounting vocabulary every caller and driver shares.
//
// # What this package is
//
// inference is the seam between a host that wants a model answer and a driver
// that knows how to obtain one. It owns vocabulary and nothing else. It never
// reads a file, resolves an environment variable, names an implementation, or
// chooses between providers: an application decides what to construct and hands
// the result here as a [Provider].
//
// # One request, one response, many modalities
//
// [GenerateRequest.Output] selects what to produce — text, image, audio, video —
// and everything else stays the same: the same request type, the same
// [GenerateResponse], the same [Provider] method. The answer is always a
// [message.Content], so the caller reads the produced parts rather than the
// produced type. A modality that lands later adds one [Output] constant, not a
// request type.
//
// # Where a knob belongs
//
// The boundary is mechanical, and it is the reason this package stays small:
//
//   - A field belongs on [GenerateRequest] when its schema is provider-neutral
//     (owned by message or by this package) AND a provider-agnostic caller sets
//     it per call. That is input, output, tools and tool choice.
//   - A value a caller sets once per deployment — sampling, response format,
//     reasoning depth, an image's size and quality, a voice — is driver
//     settings. It never crosses this boundary, and a driver applies it while
//     compiling.
//   - A value whose schema only one provider understands is an [Extension]: a
//     driver-defined, driver-decoded JSON value that other providers treat as
//     inert.
//   - Correlation data whose keys the deployment defines — a conversation id, a
//     tenant id — is not a field at all: it travels in the context as
//     [RequestMetadata], because this package does not own its schema and every
//     layer may read it.
//
// # Layers
//
//	host            Provider, request/response vocabulary, optional capabilities
//	driver author   the protocol kit in inference/protocol
//	driver tests    the conformance suites in inference/inferencetest
//
// The host face is deliberately small: a caller needs one method,
// [Provider.Generate], and, when it wants incremental output,
// [Provider.GenerateStream]. Everything a driver needs in order to be written
// and tested without a network lives in inference/protocol, which hosts do not
// import.
//
// [Catalog] is how a model's declared facts — which outputs it can produce,
// whether it calls tools, its limits — reach the runtime. Those facts are
// configuration data supplied by the application; without a reader they would be
// dead declarations.
package inference
