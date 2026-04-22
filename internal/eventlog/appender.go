package eventlog

import "context"

// Appender is the minimal write-side surface R1 publishers need.
// R2 replaces this with the full Log / UnitOfWork API from the plan.
type Appender interface {
	Append(ctx context.Context, env Envelope) (seq int64, err error)
}
