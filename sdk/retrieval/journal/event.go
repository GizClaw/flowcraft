package journal

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// Op is a journal operation type.
type Op string

const (
	OpUpsert Op = "upsert"
	OpDelete Op = "delete"
)

// Event is one persisted change.
type Event struct {
	SeqID     uint64
	Namespace string
	Op        Op
	DocID     string
	Before    *retrieval.Doc
	After     *retrieval.Doc
	Actor     string
	Timestamp time.Time
	Reason    string
}
