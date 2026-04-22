package eventlog

// PublishOption configures generated Publish* helpers (trace, actor, etc.).
type PublishOption func(*publishOpts)

type publishOpts struct {
	actor   *Actor
	traceID string
	spanID  string
}

// WithActor sets envelope.actor.
func WithActor(a Actor) PublishOption {
	return func(o *publishOpts) {
		cp := a
		o.actor = &cp
	}
}

// WithTraceIDs sets OTel trace identifiers on the envelope.
func WithTraceIDs(traceID, spanID string) PublishOption {
	return func(o *publishOpts) {
		o.traceID = traceID
		o.spanID = spanID
	}
}

func collectPublishOptions(opts []PublishOption) publishOpts {
	var o publishOpts
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	return o
}
