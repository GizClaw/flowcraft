package a2a

import (
	"net/http"
	"time"

	a2aprotocol "github.com/a2aproject/a2a-go/v2/a2a"
	"google.golang.org/grpc"
)

// StreamMode values for [Options.StreamMode].
const (
	// StreamModeAuto prefers message/stream when the remote card declares
	// streaming support and falls back to message/send + polling otherwise.
	StreamModeAuto = "auto"
	// StreamModeOn forces the streaming path (message/stream, or
	// tasks/resubscribe on resume). The SDK's client falls back to
	// message/send when the card does not advertise streaming.
	StreamModeOn = "on"
	// StreamModeOff forces the non-streaming path: message/send plus
	// tasks/get polling.
	StreamModeOff = "off"
)

// defaultPollInterval is the tasks/get polling cadence used when the caller
// does not configure one.
const defaultPollInterval = time.Second

// Options configures an [Engine]. Zero values select sensible defaults.
type Options struct {
	// ClientOptions carries the HTTP client, gRPC dial options, static
	// headers and transport preference used by the underlying A2A client.
	ClientOptions

	// StreamMode selects the execution path: "auto" (default), "on", or
	// "off". Unknown values behave as "auto".
	StreamMode string

	// PollInterval is the tasks/get polling cadence for the non-streaming
	// path. Zero selects one second.
	PollInterval time.Duration

	// HistoryLength is passed to tasks/get and the send configuration so
	// the remote includes at most this many recent history messages. Nil
	// leaves the server default in place.
	HistoryLength *int

	// AcceptedOutputModes constrains what modalities the remote agent may
	// return, mirroring A2A's acceptedOutputModes.
	AcceptedOutputModes []string
}

// Option mutates an [Options]. Nil options are ignored.
type Option func(*Options)

// WithHTTPClient overrides the HTTP client used by the JSON-RPC transports.
func WithHTTPClient(client *http.Client) Option {
	return func(o *Options) {
		if client != nil {
			o.HTTPClient = client
		}
	}
}

// WithGRPCDialOptions sets the dial options for the gRPC transports.
func WithGRPCDialOptions(dialOpts ...grpc.DialOption) Option {
	return func(o *Options) {
		o.GRPCDialOptions = append([]grpc.DialOption(nil), dialOpts...)
	}
}

// WithHeader attaches one static header to every protocol call.
func WithHeader(key, value string) Option {
	return func(o *Options) {
		if o.Headers == nil {
			o.Headers = make(map[string]string)
		}
		o.Headers[key] = value
	}
}

// WithHeaders attaches a set of static headers to every protocol call.
func WithHeaders(headers map[string]string) Option {
	return func(o *Options) {
		if len(headers) == 0 {
			return
		}
		if o.Headers == nil {
			o.Headers = make(map[string]string, len(headers))
		}
		for k, v := range headers {
			o.Headers[k] = v
		}
	}
}

// WithPreferredTransports sets the transport-selection priority. Empty
// follows the remote card's interface order.
func WithPreferredTransports(transports ...a2aprotocol.TransportProtocol) Option {
	return func(o *Options) {
		o.PreferredTransports = append([]a2aprotocol.TransportProtocol(nil), transports...)
	}
}

// WithStreamMode sets the execution path ("auto", "on", "off").
func WithStreamMode(mode string) Option {
	return func(o *Options) {
		o.StreamMode = mode
	}
}

// WithPollInterval sets the tasks/get polling cadence.
func WithPollInterval(d time.Duration) Option {
	return func(o *Options) {
		if d > 0 {
			o.PollInterval = d
		}
	}
}

// WithHistoryLength limits how much remote history each task response
// carries.
func WithHistoryLength(n int) Option {
	return func(o *Options) {
		o.HistoryLength = &n
	}
}

// WithAcceptedOutputModes constrains the modalities the remote agent may
// return.
func WithAcceptedOutputModes(modes ...string) Option {
	return func(o *Options) {
		o.AcceptedOutputModes = append([]string(nil), modes...)
	}
}

// applyOptions merges opts over the defaults.
func applyOptions(opts []Option) Options {
	out := Options{StreamMode: StreamModeAuto}
	for _, opt := range opts {
		if opt != nil {
			opt(&out)
		}
	}
	if out.PollInterval <= 0 {
		out.PollInterval = defaultPollInterval
	}
	return out
}
