package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/event"
)

// eventLog is the tour's sink on the craft plane. It prints every event
// as it arrives and lets the tour wait for one it caused, because some
// effects land on the craft's goroutines: a plugin revision reloads
// every open runtime asynchronously.
type eventLog struct {
	print *printer

	mu     sync.Mutex
	events []event.Envelope
}

func newEventLog(print *printer) *eventLog {
	return &eventLog{print: print}
}

// OnEnvelope implements event.Sink.
func (l *eventLog) OnEnvelope(_ context.Context, envelope event.Envelope) error {
	l.mu.Lock()
	l.events = append(l.events, envelope)
	l.mu.Unlock()
	l.print.event(envelope)
	return nil
}

// mark records the current end of the log.
func (l *eventLog) mark() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// wait blocks until an event at or after mark carries subject, and
// reports what was seen instead on timeout.
func (l *eventLog) wait(
	ctx context.Context,
	mark int,
	subject event.Subject,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	for {
		l.mu.Lock()
		var seen []string
		found := false
		for _, envelope := range l.events[mark:] {
			if envelope.Subject == subject {
				found = true
				break
			}
			seen = append(seen, string(envelope.Subject))
		}
		l.mu.Unlock()
		if found {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s; saw [%s]",
				subject, strings.Join(seen, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
