package retrieval

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// PartialError reports per-document outcomes for a batch Upsert.
//
// Backends return PartialError when at least one document in the batch
// failed validation but other documents were accepted (or would have been
// accepted if the backend chose to commit per-row). Inspect Results to find
// the failing IDs; entries with Err == nil indicate successful rows.
type PartialError struct {
	Results []DocUpsertResult
}

func (e *PartialError) Error() string {
	if e == nil || len(e.Results) == 0 {
		return "retrieval: partial upsert"
	}
	var b strings.Builder
	b.WriteString("retrieval: partial upsert (")
	first := true
	for _, r := range e.Results {
		if r.Err == nil {
			continue
		}
		if !first {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s: %v", r.ID, r.Err)
		first = false
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
