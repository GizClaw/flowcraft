// Package scheduler defines backend-neutral scheduling and leased-work protocols.
package scheduler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Payload is a versioned, self-describing JSON value.
type Payload struct {
	Kind    string          `json:"kind"`
	Version int             `json:"version"`
	Data    json.RawMessage `json:"data"`
}

// Validate checks that p is a complete, single JSON value.
func (p Payload) Validate() error {
	if err := required("Payload.Kind", p.Kind); err != nil {
		return err
	}
	if p.Version <= 0 {
		return invalidf("Payload.Version must be greater than zero")
	}
	if len(bytes.TrimSpace(p.Data)) == 0 {
		return invalidf("Payload.Data is required")
	}
	var value json.RawMessage
	if err := decodeStrict(p.Data, &value, false); err != nil {
		return invalidf("Payload.Data: %v", err)
	}
	return nil
}

// Task is the backend-neutral unit submitted by a scheduler client.
type Task struct {
	Payload Payload `json:"payload"`
}

// Validate checks the task payload.
func (t Task) Validate() error {
	if err := t.Payload.Validate(); err != nil {
		return invalidf("Task: %v", err)
	}
	return nil
}

// Overlap controls whether executions of one recurring rule may overlap.
type Overlap string

const (
	// OverlapSkip is the default: do not start while a prior execution is outstanding.
	OverlapSkip Overlap = ""
	// OverlapAllow permits concurrent executions of the same rule.
	OverlapAllow Overlap = "allow"
)

// Rule is the non-generic recurring schedule wire value.
type Rule struct {
	Namespace string  `json:"namespace"`
	ID        string  `json:"id"`
	Cron      string  `json:"cron"`
	Timezone  string  `json:"timezone"`
	Overlap   Overlap `json:"overlap,omitempty"`
	Task      Task    `json:"task"`
}

// Validate checks all rule fields, including the IANA timezone.
func (r Rule) Validate() error {
	if err := required("Rule.Namespace", r.Namespace); err != nil {
		return err
	}
	if err := required("Rule.ID", r.ID); err != nil {
		return err
	}
	if err := required("Rule.Cron", r.Cron); err != nil {
		return err
	}
	if r.Timezone != "" {
		if _, err := time.LoadLocation(r.Timezone); err != nil {
			return invalidf("Rule.Timezone %q is invalid: %v", r.Timezone, err)
		}
	}
	if r.Overlap != OverlapSkip && r.Overlap != OverlapAllow {
		return invalidf("Rule.Overlap %q is invalid", r.Overlap)
	}
	if err := r.Task.Validate(); err != nil {
		return invalidf("Rule.Task: %v", err)
	}
	return nil
}

// Once is a caller-identified one-shot schedule.
type Once struct {
	Namespace string    `json:"namespace"`
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Task      Task      `json:"task"`
}

// Validate checks all one-shot fields. At may be in the past for catch-up work.
func (o Once) Validate() error {
	if err := required("Once.Namespace", o.Namespace); err != nil {
		return err
	}
	if err := required("Once.ID", o.ID); err != nil {
		return err
	}
	if o.At.IsZero() {
		return invalidf("Once.At is required")
	}
	if err := o.Task.Validate(); err != nil {
		return invalidf("Once.Task: %v", err)
	}
	return nil
}

// Delivery is a leased execution handed to a worker.
type Delivery struct {
	ID          string    `json:"id"`
	ExecutionID string    `json:"executionId"`
	Namespace   string    `json:"namespace"`
	ScheduleID  string    `json:"scheduleId"`
	Task        Task      `json:"task"`
	Attempt     int       `json:"attempt"`
	LeaseToken  string    `json:"leaseToken"`
	LeaseUntil  time.Time `json:"leaseUntil"`
	ScheduledAt time.Time `json:"scheduledAt"`
}

// Validate checks delivery identity, lease, timing, and task fields.
func (d Delivery) Validate() error {
	for field, value := range map[string]string{
		"Delivery.ID":          d.ID,
		"Delivery.ExecutionID": d.ExecutionID,
		"Delivery.Namespace":   d.Namespace,
		"Delivery.ScheduleID":  d.ScheduleID,
		"Delivery.LeaseToken":  d.LeaseToken,
	} {
		if err := required(field, value); err != nil {
			return err
		}
	}
	if d.Attempt <= 0 {
		return invalidf("Delivery.Attempt must be greater than zero")
	}
	if d.LeaseUntil.IsZero() {
		return invalidf("Delivery.LeaseUntil is required")
	}
	if d.ScheduledAt.IsZero() {
		return invalidf("Delivery.ScheduledAt is required")
	}
	if err := d.Task.Validate(); err != nil {
		return invalidf("Delivery.Task: %v", err)
	}
	return nil
}

// Status is the durable execution lifecycle state.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

// Outstanding reports whether more work may occur for the execution.
func (s Status) Outstanding() bool { return s == StatusQueued || s == StatusRunning }

// IsOutstanding is an explicit predicate alias for Outstanding.
func (s Status) IsOutstanding() bool { return s.Outstanding() }

// Terminal reports whether no more work may occur for the execution.
func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCanceled
}

// IsTerminal is an explicit predicate alias for Terminal.
func (s Status) IsTerminal() bool { return s.Terminal() }

// Valid reports whether s is a defined scheduler status.
func (s Status) Valid() bool { return s.Outstanding() || s.Terminal() }

// Execution is the durable view of one schedule firing.
type Execution struct {
	ID          string     `json:"id"`
	Namespace   string     `json:"namespace"`
	ScheduleID  string     `json:"scheduleId"`
	Task        Task       `json:"task"`
	Status      Status     `json:"status"`
	Attempt     int        `json:"attempt"`
	ScheduledAt time.Time  `json:"scheduledAt"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// Validate checks execution identity, lifecycle state, timing, and task fields.
func (e Execution) Validate() error {
	for field, value := range map[string]string{
		"Execution.ID":         e.ID,
		"Execution.Namespace":  e.Namespace,
		"Execution.ScheduleID": e.ScheduleID,
	} {
		if err := required(field, value); err != nil {
			return err
		}
	}
	if !e.Status.Valid() {
		return invalidf("Execution.Status %q is invalid", e.Status)
	}
	if e.Attempt < 0 {
		return invalidf("Execution.Attempt must not be negative")
	}
	if e.ScheduledAt.IsZero() {
		return invalidf("Execution.ScheduledAt is required")
	}
	if e.Status == StatusRunning && e.StartedAt == nil {
		return invalidf("Execution.StartedAt is required for running status")
	}
	if e.Status.Terminal() && e.FinishedAt == nil {
		return invalidf("Execution.FinishedAt is required for terminal status")
	}
	if err := e.Task.Validate(); err != nil {
		return invalidf("Execution.Task: %v", err)
	}
	return nil
}

// ClaimRequest asks for one available execution in a namespace.
type ClaimRequest struct {
	Namespace     string        `json:"namespace"`
	LeaseDuration time.Duration `json:"leaseDuration"`
}

// Validate checks claim scope and lease duration.
func (r ClaimRequest) Validate() error {
	if err := required("ClaimRequest.Namespace", r.Namespace); err != nil {
		return err
	}
	if r.LeaseDuration <= 0 {
		return invalidf("ClaimRequest.LeaseDuration must be greater than zero")
	}
	return nil
}

// RenewRequest extends a lease held by execution ID and opaque lease token.
type RenewRequest struct {
	ExecutionID   string        `json:"executionId"`
	LeaseToken    string        `json:"leaseToken"`
	LeaseDuration time.Duration `json:"leaseDuration"`
}

// Validate checks renewal identity and duration.
func (r RenewRequest) Validate() error {
	if err := required("RenewRequest.ExecutionID", r.ExecutionID); err != nil {
		return err
	}
	if err := required("RenewRequest.LeaseToken", r.LeaseToken); err != nil {
		return err
	}
	if r.LeaseDuration <= 0 {
		return invalidf("RenewRequest.LeaseDuration must be greater than zero")
	}
	return nil
}

// CompleteRequest settles a leased execution at a business terminal state.
// RetainUntil optionally asks the server to preserve exact-request idempotency
// through that UTC instant. Servers may reject deadlines beyond a documented
// maximum.
type CompleteRequest struct {
	ExecutionID string     `json:"executionId"`
	LeaseToken  string     `json:"leaseToken"`
	Status      Status     `json:"status"`
	Error       string     `json:"error,omitempty"`
	RetainUntil *time.Time `json:"retainUntil,omitempty"`
}

// Validate accepts only terminal statuses.
func (r CompleteRequest) Validate() error {
	if err := required("CompleteRequest.ExecutionID", r.ExecutionID); err != nil {
		return err
	}
	if err := required("CompleteRequest.LeaseToken", r.LeaseToken); err != nil {
		return err
	}
	if !r.Status.Terminal() {
		return invalidf("CompleteRequest.Status %q must be terminal", r.Status)
	}
	if r.RetainUntil != nil && r.RetainUntil.Location() != time.UTC {
		return invalidf("CompleteRequest.RetainUntil must use UTC")
	}
	return nil
}

func required(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return invalidf("%s is required", field)
	}
	if strings.ContainsRune(value, '\x00') {
		return invalidf("%s must not contain NUL", field)
	}
	return nil
}

func invalidf(format string, args ...any) error {
	return errdefs.Validation(fmt.Errorf("scheduler: "+format, args...))
}
