package retrieval

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// PartialError reports per-document outcomes for a batch Upsert.
type PartialError struct {
	Results []DocUpsertResult
}

func (e *PartialError) Error() string {
	var b strings.Builder
	b.WriteString("retrieval: partial upsert (")
	for i, r := range e.Results {
		if i > 0 {
			b.WriteString("; ")
		}
		if r.Err != nil {
			fmt.Fprintf(&b, "%s: %v", r.ID, r.Err)
		}
	}
	b.WriteString(")")
	return b.String()
}

// DocUpsertResult is one row in a PartialError.
type DocUpsertResult struct {
	ID  string
	Err error
}

// ErrEmptyDeleteFilter is returned when DeleteByFilter is called with an empty filter.
var ErrEmptyDeleteFilter = errdefs.Validationf("retrieval: delete by filter requires a non-empty filter")

// ErrNoQuery is returned when SearchRequest has no query signal.
var ErrNoQuery = errdefs.Validationf("retrieval: search requires at least one of query_text, query_vector, or sparse_vec")
