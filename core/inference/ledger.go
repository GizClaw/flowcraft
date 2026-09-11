package inference

import (
	"fmt"
	"strings"
)

// Ledger tracks one compile's active fields and the dispositions the compiler
// assigns them, so a request never loses a setting silently: every active field
// ends the compile as Native, Dropped, or Rejected, which is exactly what
// [CompileReport.ValidateSuccess] and [CompileReport.ValidateFailure] require.
//
// It exists so drivers do not each re-implement the same bookkeeping. Build one
// with the operation, the compiler's provider label (used in rejection errors),
// and the request's active fields; record Drop/Reject decisions as the compile
// makes them; then hand Report to the runtime and Err to the caller.
type Ledger struct {
	operation Operation
	provider  string
	active    []FieldID
	rejected  map[FieldID]string
	dropped   map[FieldID]string
	// components carries per-component notes for fields whose drop needs to
	// explain which components of an aggregated field reached the wire.
	components map[FieldID][]ComponentNote
	// order is the rejection order, so Err reports the first field the
	// compiler refused rather than a map iteration order.
	order []FieldID
}

// NewLedger starts a ledger for one compile over active. provider labels the
// errors Err builds; it is the driver's own name (for example "openai").
func NewLedger(operation Operation, provider string, active []FieldID) *Ledger {
	return &Ledger{
		operation:  operation,
		provider:   provider,
		active:     append([]FieldID(nil), active...),
		rejected:   make(map[FieldID]string),
		dropped:    make(map[FieldID]string),
		components: make(map[FieldID][]ComponentNote),
	}
}

// Reject records a field the compiler cannot honor and fails the compile. The
// first rejected field, in rejection order, is the field Err reports.
func (l *Ledger) Reject(field FieldID, reason string) {
	if l == nil {
		return
	}
	if _, exists := l.rejected[field]; !exists {
		l.order = append(l.order, field)
		l.rejected[field] = reason
	}
}

// Drop records an intentional discard that keeps the compile successful.
// Rejection wins when both land on one field: a failed compile reports the
// rejection, not the drop. Dropping never clears component notes recorded by
// [Ledger.DropComponents], and the first reason recorded for a field is the one
// the report carries.
func (l *Ledger) Drop(field FieldID, reason string) {
	if l == nil {
		return
	}
	if _, rejected := l.rejected[field]; rejected {
		return
	}
	if _, exists := l.dropped[field]; !exists {
		l.dropped[field] = reason
	}
}

// DropComponents records a drop together with the per-component notes that
// explain it: which components of an aggregated field reached the wire and
// which did not. Empty notes degrade to [Ledger.Drop].
//
// The notes are carried on the field's own decision, so they must fold onto the
// drop the way [CompileReport] validates them: a decision's component
// dispositions fold to its disposition, so a dropped field needs at least one
// Dropped note and may not carry a Rejected one. A driver that records notes
// without a degraded component produces a report the runtime rejects as a
// contract violation rather than one the provider sees.
//
// Notes only ever attach to drops: a rejected field never reaches the wire, so
// there is nothing to say about its components. (The report shape itself allows
// components on a rejection; no driver needs that today, so the ledger does not
// offer it.)
func (l *Ledger) DropComponents(
	field FieldID,
	notes []ComponentNote,
	reason string,
) {
	if l == nil {
		return
	}
	if len(notes) > 0 {
		l.components[field] = append(l.components[field], notes...)
	}
	l.Drop(field, reason)
}

// Rejected reports whether any field was rejected.
func (l *Ledger) Rejected() bool {
	return l != nil && len(l.order) > 0
}

// Report renders the compile report: every active field carries exactly one
// disposition — Rejected, then Dropped, otherwise Native — in the order the
// fields were declared.
func (l *Ledger) Report() CompileReport {
	if l == nil {
		return CompileReport{}
	}
	decisions := make([]Decision, 0, len(l.active))
	for _, field := range l.active {
		switch {
		case l.rejected[field] != "":
			decisions = append(decisions, Decision{
				Field:       field,
				Disposition: Rejected,
				Reason:      l.rejected[field],
			})
		case l.dropped[field] != "":
			decisions = append(decisions, Decision{
				Field:       field,
				Disposition: Dropped,
				Reason:      l.dropped[field],
				Components:  append([]ComponentNote(nil), l.components[field]...),
			})
		default:
			decisions = append(decisions, Decision{
				Field:       field,
				Disposition: Native,
			})
		}
	}
	return CompileReport{Operation: l.operation, Decisions: decisions}
}

// Err builds the structured compiler rejection for the first rejected field:
// extension fields classify as InvalidExtension (the extension is invalid for
// this provider), everything else as UnsupportedFeature (this model cannot
// serve that request setting). It returns nil when nothing was rejected.
func (l *Ledger) Err() error {
	if l == nil || len(l.order) == 0 {
		return nil
	}
	field := l.order[0]
	kind := UnsupportedFeature
	if strings.HasPrefix(string(field), "extension.") {
		kind = InvalidExtension
	}
	return NewError(
		kind,
		l.operation,
		field,
		fmt.Errorf("%s: %s", l.provider, l.rejected[field]),
	)
}
