package retrieval

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ErrEmptyDeleteFilter is returned when DeleteByFilter is called with an empty filter.
var ErrEmptyDeleteFilter = errdefs.Validationf("retrieval: delete by filter requires a non-empty filter")

// ErrNoQuery is returned when SearchRequest has no query signal.
var ErrNoQuery = errdefs.Validationf("retrieval: search requires at least one of query_text, query_vector, or sparse_vec")
