package extract

import "context"

type contextKey string

const extractorKey contextKey = "extractor"

// WithExtractor adds an Extractor to the context.
func WithExtractor(ctx context.Context, e Extractor) context.Context {
	return context.WithValue(ctx, extractorKey, e)
}

// ExtractorFrom retrieves an Extractor from the context.
func ExtractorFrom(ctx context.Context) (Extractor, bool) {
	e, ok := ctx.Value(extractorKey).(Extractor)
	return e, ok
}
