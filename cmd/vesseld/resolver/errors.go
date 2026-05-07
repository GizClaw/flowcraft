package resolver

import (
	"errors"
	"strings"
)

// Errors aggregates the issues Resolve found across the entire
// input set. The resolver never stops at the first failure: users
// running `vesseld validate` see every problem in one report and
// can fix them all at once instead of playing whack-a-mole.
//
// Errors implements the error interface so callers can return it
// directly. The Aggregate() helper joins via [errors.Join] for
// callers that want errors.Is / errors.As inspection of the
// individual entries.
type Errors struct {
	items []error
}

// add is the package-internal append. Nil errors are ignored so
// helpers can call e.add(maybeErr) unconditionally.
func (e *Errors) add(err error) {
	if err == nil {
		return
	}
	e.items = append(e.items, err)
}

// addAll concatenates another Errors aggregate into this one.
func (e *Errors) addAll(other *Errors) {
	if other == nil {
		return
	}
	e.items = append(e.items, other.items...)
}

// Len returns how many problems are recorded.
func (e *Errors) Len() int { return len(e.items) }

// All returns the underlying error slice. Returned slice is not
// safe to mutate by callers (it shares backing storage with the
// aggregate); copy before modifying.
func (e *Errors) All() []error { return e.items }

// Aggregate returns errors.Join over every entry, or nil if empty.
// Use this when handing the aggregate to a caller that wants
// standard error inspection.
func (e *Errors) Aggregate() error {
	if e == nil || len(e.items) == 0 {
		return nil
	}
	return errors.Join(e.items...)
}

// Error implements the error interface. The output lists every
// problem on its own line with a leading "  - " bullet so logs and
// CLI output read cleanly.
func (e *Errors) Error() string {
	if e == nil || len(e.items) == 0 {
		return ""
	}
	if len(e.items) == 1 {
		return e.items[0].Error()
	}
	var sb strings.Builder
	sb.WriteString("vesseld: configuration has ")
	sb.WriteString(itoa(len(e.items)))
	sb.WriteString(" problems:\n")
	for _, err := range e.items {
		sb.WriteString("  - ")
		sb.WriteString(err.Error())
		sb.WriteString("\n")
	}
	return sb.String()
}

// itoa is a tiny inline helper avoiding an strconv import for the
// single use in Error().
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
