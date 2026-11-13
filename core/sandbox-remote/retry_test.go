package remote

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox-remote/wire"
)

// The retry policy is observable here because both halves — how long to wait and
// the waiting itself — are variables. A test that slept through the real schedule
// would be slow and would assert nothing about it.

// scriptedTransport answers with the errors it was given, in order.
type scriptedTransport struct {
	errs  []error
	calls int
}

func (s *scriptedTransport) Do(context.Context, Op, []byte, any) error {
	s.calls++
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

func (s *scriptedTransport) Stream(context.Context, Op, []byte) (io.ReadCloser, error) {
	return nil, errdefs.NotAvailablef("scriptedTransport: no stream")
}

// recordSleeps replaces the wait with a recording one.
func recordSleeps(t *testing.T) *[]time.Duration {
	t.Helper()
	delays := &[]time.Duration{}
	prevSleep := retrySleep
	prevBackoff := retryBackoff
	retrySleep = func(_ context.Context, d time.Duration) error {
		*delays = append(*delays, d)
		return nil
	}
	retryBackoff = func(attempt int) time.Duration { return time.Duration(attempt+1) * time.Millisecond }
	t.Cleanup(func() {
		retrySleep = prevSleep
		retryBackoff = prevBackoff
	})
	return delays
}

func newScriptedRunner(transport Transport) *Runner {
	return &Runner{
		transport: transport,
		hold:      time.Second,
		retries:   defaultRetries,
		sessions:  map[string]*session{},
	}
}

func transportFailure() error {
	return &TransportError{Err: errors.New("connection refused")}
}

// A retried operation waits between attempts, and the last attempt does not wait
// after itself.
func TestRetryWaitsBetweenTransportFailures(t *testing.T) {
	delays := recordSleeps(t)
	tr := &scriptedTransport{errs: []error{transportFailure(), transportFailure()}}
	r := newScriptedRunner(tr)

	var out wire.ReadResponse
	if err := r.retry(context.Background(), OpRead, nil, &out); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if tr.calls != 3 {
		t.Fatalf("attempts = %d, want 3", tr.calls)
	}
	if len(*delays) != 2 {
		t.Fatalf("waits = %v, want one before each retry and none after the last", *delays)
	}
	for i, d := range *delays {
		if d != time.Duration(i+1)*time.Millisecond {
			t.Fatalf("wait %d = %v, want the schedule's value", i, d)
		}
	}
}

// An answer from the server is final: waiting would only delay the report.
func TestRetryDoesNotWaitForServerAnswers(t *testing.T) {
	delays := recordSleeps(t)
	tr := &scriptedTransport{errs: []error{errdefs.NotAvailablef("no sessions here")}}
	r := newScriptedRunner(tr)

	var out wire.ReadResponse
	err := r.retry(context.Background(), OpRead, nil, &out)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("retry = %v, want NotAvailable", err)
	}
	if tr.calls != 1 {
		t.Fatalf("attempts = %d, want 1", tr.calls)
	}
	if len(*delays) != 0 {
		t.Fatalf("waited %v for a server answer", *delays)
	}
}

// A caller that gives up while we are sleeping gets its own cancellation reported,
// not the transport failure that made us sleep.
func TestRetryReportsCancellationDuringBackoff(t *testing.T) {
	prevSleep := retrySleep
	prevBackoff := retryBackoff
	retryBackoff = func(int) time.Duration { return time.Millisecond }
	retrySleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() {
		retrySleep = prevSleep
		retryBackoff = prevBackoff
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr := &scriptedTransport{errs: []error{transportFailure(), transportFailure()}}
	r := newScriptedRunner(tr)

	var out wire.ReadResponse
	err := r.retry(ctx, OpRead, nil, &out)
	if !errors.Is(err, context.Canceled) && !errdefs.IsAborted(err) {
		t.Fatalf("retry = %v, want the caller's cancellation", err)
	}
	if tr.calls != 1 {
		t.Fatalf("attempts = %d; a cancelled caller must not keep trying", tr.calls)
	}
}

// The schedule grows, is bounded, and the budget constant describes it.
func TestRetrySchedule(t *testing.T) {
	var previous time.Duration
	var total time.Duration
	for attempt := range defaultRetries {
		d := backoffFor(attempt)
		if d <= previous {
			t.Fatalf("attempt %d waits %v, not more than %v", attempt, d, previous)
		}
		previous = d
		total += d
	}
	if total > retryBudget {
		t.Fatalf("the schedule adds up to %v, over the documented %v", total, retryBudget)
	}
	if last := backoffFor(100); last > time.Second {
		t.Fatalf("the schedule grows past a second: %v", last)
	}
}

// Jitter only ever lengthens a wait, so the "do not hammer" property survives every
// draw, and it stays inside the documented twenty percent.
func TestRetryBackoffJitterStaysInRange(t *testing.T) {
	for attempt := range defaultRetries {
		base := backoffFor(attempt)
		for range 100 {
			got := retryBackoff(attempt)
			if got < base || got > base+base/5 {
				t.Fatalf("attempt %d: jittered %v outside [%v, %v]", attempt, got, base, base+base/5)
			}
		}
	}
}
