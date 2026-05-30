package bm25

// scoreConfig captures the tuneable BM25 parameters. The zero value
// is not a valid configuration; callers must go through
// [applyScoreOptions] to get the textbook defaults seeded.
type scoreConfig struct {
	k1, b float64
}

// ScoreOption configures BM25 scoring parameters. The two knobs
// the canonical Okapi BM25 paper exposes are k1 (term-frequency
// saturation) and b (length-normalisation strength); both default
// to the textbook values when omitted.
type ScoreOption func(*scoreConfig)

// WithK1 sets the BM25 term frequency saturation parameter
// (default 1.2). Higher values give diminishing-returns repeats
// more weight; lower values flatten tf influence.
func WithK1(k1 float64) ScoreOption {
	return func(c *scoreConfig) { c.k1 = k1 }
}

// WithB sets the BM25 length normalization strength (default
// 0.75). 0 disables length normalisation entirely (favouring
// long documents); 1 fully normalises (favouring short documents).
func WithB(b float64) ScoreOption {
	return func(c *scoreConfig) { c.b = b }
}

func applyScoreOptions(opts []ScoreOption) scoreConfig {
	c := scoreConfig{k1: 1.2, b: 0.75}
	for _, o := range opts {
		o(&c)
	}
	return c
}
