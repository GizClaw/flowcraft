package kanban

import (
	"maps"
	"time"

	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdkx/delegation"
)

// Status is the internal lifecycle state of a delegation card.
type Status string

const (
	StatusPending   Status = "pending"
	StatusClaimed   Status = "claimed"
	StatusSuspended Status = "suspended"
	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// IsTerminal reports whether no further transition is permitted.
func (s Status) IsTerminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCancelled
}

// Task is the immutable asynchronous delegation stored by a card.
type Task struct {
	Request delegation.AsyncRequest `json:"request"`
}

// Result is the terminal delegation response stored by a card.
type Result struct {
	Response sdkdelegation.Response `json:"response"`
}

// Card is an operational snapshot of one delegation backend entry.
// Backend callers normally use only the id returned by Submit and Status.
type Card struct {
	ID        string            `json:"id"`
	Producer  string            `json:"producer,omitempty"`
	Consumer  string            `json:"consumer,omitempty"`
	Status    Status            `json:"status"`
	Task      *Task             `json:"task"`
	Result    *Result           `json:"result,omitempty"`
	ResumeRef string            `json:"resume_ref,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Meta      map[string]string `json:"meta,omitempty"`
}

// Elapsed reports the time from submission to the latest transition.
func (c *Card) Elapsed() time.Duration {
	if c == nil || c.UpdatedAt.Before(c.CreatedAt) {
		return 0
	}
	return c.UpdatedAt.Sub(c.CreatedAt)
}

// Filter selects operational card snapshots. Empty fields are wildcards.
type Filter struct {
	Producer string
	Consumer string
	Status   Status
	Target   string
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
	return f.Target == "" ||
		(c.Task != nil && c.Task.Request.Request.Target == f.Target)
}

func (c *Card) clone() *Card {
	if c == nil {
		return nil
	}
	out := *c
	if c.Task != nil {
		out.Task = &Task{Request: cloneAsyncRequest(c.Task.Request)}
	}
	if c.Result != nil {
		out.Result = &Result{Response: cloneResponse(c.Result.Response)}
	}
	out.Meta = cloneMetadata(c.Meta)
	return &out
}

func cloneAsyncRequest(req delegation.AsyncRequest) delegation.AsyncRequest {
	req.Request.Metadata = cloneMetadata(req.Request.Metadata)
	return req
}

func cloneResponse(response sdkdelegation.Response) sdkdelegation.Response {
	response.Metadata = cloneMetadata(response.Metadata)
	return response
}

func cloneMetadata(metadata map[string]string) map[string]string {
	return maps.Clone(metadata)
}
