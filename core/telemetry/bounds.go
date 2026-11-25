package telemetry

import "slices"

// durationBoundsSeconds is the bucket layout for a duration recorded in seconds.
//
// The SDK's default boundaries are built for milliseconds — 0, 5, 10, 25, ... — so a
// histogram of seconds left with them reports every call of five seconds or more as one
// bucket, and a p99 read off that histogram is a guess. These are the boundaries a
// seconds-valued latency distribution actually spreads over: tens of milliseconds for a
// cache hit, single seconds for a model call, and the tail a streaming request can reach.
var durationBoundsSeconds = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10, 30, 60,
}

// DurationBoundsSeconds returns the bucket boundaries for a duration recorded in seconds.
//
// It returns a copy: the boundaries are read once, when an instrument is created, and a
// caller that sorted or extended the slice it was given would silently change the
// histogram every other package declares.
func DurationBoundsSeconds() []float64 {
	return slices.Clone(durationBoundsSeconds)
}
