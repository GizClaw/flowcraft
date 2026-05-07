package vesselquality

import "errors"

// errFakeWorker is reused across chaos / kanban tests where we
// want a sentinel "the worker LLM exploded" the assertions can
// distinguish from generic infra errors.
var errFakeWorker = errors.New("fakellm: worker exploded")
