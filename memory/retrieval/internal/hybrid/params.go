package hybrid

import (
	"math"
	"slices"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// NormalizeMode resolves HybridDefault and validates the requested mode.
func NormalizeMode(mode retrieval.HybridMode) (retrieval.HybridMode, error) {
	switch mode {
	case retrieval.HybridDefault:
		return retrieval.HybridRRF, nil
	case retrieval.HybridRRF, retrieval.HybridWeighted, retrieval.HybridConvex:
		return mode, nil
	default:
		return "", errdefs.Validationf("retrieval: unsupported hybrid mode %q", mode)
	}
}

// RRFK validates HybridOptions.K for RRF. Zero is interpreted by the scorer as
// its default damping constant.
func RRFK(options retrieval.HybridOptions) (float64, error) {
	if len(options.Weights) > 0 {
		return 0, errdefs.Validationf("retrieval: hybrid rrf does not accept weights")
	}
	if options.Alpha != nil {
		return 0, errdefs.Validationf("retrieval: hybrid rrf does not accept alpha")
	}
	k := options.K
	if k < 0 || math.IsNaN(k) || math.IsInf(k, 0) {
		return 0, errdefs.Validationf("retrieval: hybrid rrf k must be finite and non-negative")
	}
	return k, nil
}

// Weights parses raw weighted-fusion weights. Missing signal weights default to
// 1.0.
func Weights(options retrieval.HybridOptions, signals []retrieval.SearchSignal) (map[string]float64, error) {
	if options.K != 0 {
		return nil, errdefs.Validationf("retrieval: hybrid weighted does not accept rrf k")
	}
	if options.Alpha != nil {
		return nil, errdefs.Validationf("retrieval: hybrid weighted does not accept alpha")
	}
	return parseWeights(options, signals, false)
}

// ConvexWeights parses convex-fusion weights and normalizes them to sum to 1.0.
// Callers may use HybridOptions.Alpha for two-signal BM25+vector fusion: alpha
// is the BM25 weight and 1-alpha is the vector weight.
func ConvexWeights(options retrieval.HybridOptions, signals []retrieval.SearchSignal) (map[string]float64, error) {
	if options.K != 0 {
		return nil, errdefs.Validationf("retrieval: hybrid convex does not accept rrf k")
	}
	if options.Alpha != nil && len(options.Weights) > 0 {
		return nil, errdefs.Validationf("retrieval: hybrid convex accepts alpha or weights, not both")
	}
	if options.Alpha != nil {
		alpha := *options.Alpha
		if alpha < 0 || alpha > 1 || math.IsNaN(alpha) || math.IsInf(alpha, 0) {
			return nil, errdefs.Validationf("retrieval: hybrid alpha must be in [0,1]")
		}
		if !sameSignals(signals, []retrieval.SearchSignal{retrieval.SearchSignalBM25, retrieval.SearchSignalVector}) {
			return nil, errdefs.Validationf("retrieval: hybrid alpha is only supported for bm25+vector fusion")
		}
		return map[string]float64{string(retrieval.SearchSignalBM25): alpha, string(retrieval.SearchSignalVector): 1 - alpha}, nil
	}
	return parseWeights(options, signals, true)
}

func parseWeights(options retrieval.HybridOptions, signals []retrieval.SearchSignal, convex bool) (map[string]float64, error) {
	out := make(map[string]float64, len(signals))
	active := make(map[retrieval.SearchSignal]struct{}, len(signals))
	for _, signal := range signals {
		if !validSearchSignal(signal) {
			return nil, errdefs.Validationf("retrieval: unsupported search signal %q", signal)
		}
		active[signal] = struct{}{}
		out[string(signal)] = 1
	}

	for signal, weight := range options.Weights {
		if !validSearchSignal(signal) {
			return nil, errdefs.Validationf("retrieval: unsupported search signal %q", signal)
		}
		if _, ok := active[signal]; !ok {
			return nil, errdefs.Validationf("retrieval: hybrid weight %q does not match an active search signal", signal)
		}
		if weight < 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
			return nil, errdefs.Validationf("retrieval: hybrid weight %q must be finite and non-negative", signal)
		}
		out[string(signal)] = weight
	}

	var total float64
	for _, signal := range signals {
		total += out[string(signal)]
	}
	if total <= 0 {
		if convex {
			return nil, errdefs.Validationf("retrieval: convex hybrid weights must contain a positive weight")
		}
		return nil, errdefs.Validationf("retrieval: weighted hybrid weights must contain a positive weight")
	}
	if convex {
		for _, signal := range signals {
			out[string(signal)] /= total
		}
	}
	return out, nil
}

func validSearchSignal(signal retrieval.SearchSignal) bool {
	switch signal {
	case retrieval.SearchSignalBM25, retrieval.SearchSignalVector, retrieval.SearchSignalSparse:
		return true
	default:
		return false
	}
}

func sameSignals(a, b []retrieval.SearchSignal) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]retrieval.SearchSignal(nil), a...)
	bb := append([]retrieval.SearchSignal(nil), b...)
	slices.Sort(aa)
	slices.Sort(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
