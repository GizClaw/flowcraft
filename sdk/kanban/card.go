package kanban

import "time"

// Status is the lifecycle state of a [Card].
//
// The state machine is small on purpose. Every transition is a method
// on [Kanban] and every method is compare-and-swap: it inspects the
// current status and returns false if the card is not in the state the
// transition requires. Several workers can therefore race on the same
// card and exactly one wins.
//
//	Pending ──Claim──▶ Claimed ──Done───▶ Done
//	   ▲                  │
//	   │                  ├───Fail───▶ Failed
//	   │                  │
//	   └──Resume── Suspended ◀─Suspend─┘
//
//	Cancel: Pending | Claimed | Suspended ──▶ Cancelled
//
// Done, Failed and Cancelled are terminal: no further transition is
// accepted and the card becomes eligible for eviction. Suspended is
// NOT terminal — it is the parking state for work that is waiting on
// something outside the board.
type Status string

const (
	// StatusPending is a submitted card no worker has taken yet.
	StatusPending Status = "pending"

	// StatusClaimed means a worker holds the card and is executing it.
	StatusClaimed Status = "claimed"

	// StatusSuspended means execution stopped mid-flight and is
	// expected to continue later — an engine interrupt awaiting a
	// human answer, an approval gate, a checkpoint waiting to be
	// resumed. The card no longer counts as work in progress, so it
	// does not hold a worker slot, but it is not finished either.
	//
	// The board deliberately does not know what the card is waiting
	// for. [Kanban.Suspend] takes an opaque resume reference that the
	// executor interprets — typically a checkpoint id it can hand
	// back to its engine.
	StatusSuspended Status = "suspended"

	// StatusDone means the card completed and Result carries the output.
	StatusDone Status = "done"

	// StatusFailed means execution errored; Result.Error explains it.
	StatusFailed Status = "failed"

	// StatusCancelled means the card was deliberately abandoned. It is
	// distinct from StatusFailed so failure-rate dashboards stay
	// honest across orderly shutdowns.
	StatusCancelled Status = "cancelled"
)

// IsTerminal reports whether s admits no further transition. Suspended
// is not terminal: it is waiting, not finished.
func (s Status) IsTerminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCancelled
}

// Task is the work a card requests. It is written once at submit time
// and never mutated afterwards, so any reader — a worker, a dashboard,
// an agent asking what it was told to do — sees identical bytes.
type Task struct {
	// TargetAgentID names the agent expected to execute this task.
	// The board treats it as an opaque routing key: it never resolves
	// the id and never checks that such an agent exists. Supply a
	// [Validator] if you want submission to fail fast on unknown ids.
	TargetAgentID string `json:"target_agent_id"`

	// Query is the instruction for the target agent.
	Query string `json:"query"`

	// Inputs are structured parameters accompanying Query.
	Inputs map[string]any `json:"inputs,omitempty"`

	// UserQuery is the original human request that led to this task,
	// preserved so the executing agent can see the intent behind its
	// instruction rather than only the instruction.
	UserQuery string `json:"user_query,omitempty"`

	// DispatchNote is the submitting agent's own note to its future
	// self: why it delegated, what it expects back.
	DispatchNote string `json:"dispatch_note,omitempty"`

	// Timeout bounds how long execution may take. Zero means no
	// board-imposed bound.
	//
	// This is a declaration, not an enforcement: the board never runs
	// anything and therefore cannot time anything out. Executors are
	// expected to honour it, conventionally by deriving a
	// context.WithTimeout before running and calling
	// [Kanban.Fail] when it expires.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// Result is the outcome of a card, written by the transition that
// moved it to a terminal state. Exactly one of Output or Error is
// meaningful, decided by the card's [Status].
type Result struct {
	// Output is the successful result, set on StatusDone.
	Output string `json:"output,omitempty"`

	// Error explains a non-success outcome: the executor's error on
	// StatusFailed, the reason on StatusCancelled.
	Error string `json:"error,omitempty"`
}

// Card is one unit of work on the board, plus its lifecycle state.
//
// Task and Result are separate typed fields rather than one
// polymorphic payload. The distinction matters: Task is the immutable
// request, Result is the mutable outcome, and a reader always knows
// which one it is holding without a type assertion.
type Card struct {
	// ID is the board-assigned identifier, unique within a [Kanban].
	ID string `json:"id"`

	// Producer identifies who submitted the card, taken from
	// [ProducerIDFrom] on the submitting context.
	Producer string `json:"producer,omitempty"`

	// Consumer identifies the worker that claimed the card. Empty
	// until [Kanban.Claim] succeeds.
	Consumer string `json:"consumer,omitempty"`

	// Status is the current lifecycle state.
	Status Status `json:"status"`

	// Task is the requested work. Never nil.
	Task *Task `json:"task"`

	// Result is the outcome. Nil until the card reaches a terminal
	// state.
	Result *Result `json:"result,omitempty"`

	// ResumeRef is the opaque reference recorded by [Kanban.Suspend]
	// and consumed by whoever resumes the card. Empty unless the card
	// is or has been suspended. The board never interprets it.
	ResumeRef string `json:"resume_ref,omitempty"`

	// CreatedAt is when the card was submitted.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is when its status last changed.
	UpdatedAt time.Time `json:"updated_at"`

	// Meta carries caller-defined labels. The board copies it but
	// never reads it, so callers can correlate cards with schedules,
	// conversations, or tenants without the board growing a field per
	// use case.
	Meta map[string]string `json:"meta,omitempty"`
}

// Elapsed reports how long the card took to reach its current status.
func (c *Card) Elapsed() time.Duration {
	if c == nil || c.UpdatedAt.Before(c.CreatedAt) {
		return 0
	}
	return c.UpdatedAt.Sub(c.CreatedAt)
}

// Filter selects cards by exact match. Zero-value fields are
// wildcards, so the zero Filter matches every card.
type Filter struct {
	Producer string
	Consumer string
	Status   Status
	// TargetAgentID matches against Task.TargetAgentID.
	TargetAgentID string
}

func (f Filter) matches(c *Card) bool {
	if c == nil {
		return false
	}
	if f.Producer != "" && c.Producer != f.Producer {
		return false
	}
	if f.Consumer != "" && c.Consumer != f.Consumer {
		return false
	}
	if f.Status != "" && c.Status != f.Status {
		return false
	}
	if f.TargetAgentID != "" &&
		(c.Task == nil || c.Task.TargetAgentID != f.TargetAgentID) {
		return false
	}
	return true
}

// SubmitOption configures a card at submit time.
type SubmitOption func(*Card)

// WithMeta attaches one key-value label to the card.
func WithMeta(k, v string) SubmitOption {
	return func(c *Card) {
		if c.Meta == nil {
			c.Meta = make(map[string]string)
		}
		c.Meta[k] = v
	}
}

// WithTimestamps overrides the creation and update times the board
// would otherwise stamp with time.Now(). A zero argument keeps the
// default for that field.
//
// This exists for restoring persisted cards: rebuilding a board after
// a restart must preserve original timings or every latency metric
// and timeline view resets.
func WithTimestamps(created, updated time.Time) SubmitOption {
	return func(c *Card) {
		if !created.IsZero() {
			c.CreatedAt = created
		}
		if !updated.IsZero() {
			c.UpdatedAt = updated
		}
	}
}

// clone returns a deep copy so callers can never mutate board state
// through a returned pointer.
func (c *Card) clone() *Card {
	if c == nil {
		return nil
	}
	cp := *c
	if c.Task != nil {
		task := *c.Task
		task.Inputs = cloneInputs(c.Task.Inputs)
		cp.Task = &task
	}
	if c.Result != nil {
		result := *c.Result
		cp.Result = &result
	}
	cp.Meta = cloneMeta(c.Meta)
	return &cp
}

func cloneMeta(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// cloneInputs copies the top level of an Inputs map. Values are
// shared: they are caller-owned data of arbitrary type, and a deep
// copy would require either reflection or a JSON round trip that
// silently rewrites types. Callers must not mutate values they have
// already submitted.
func cloneInputs(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}
