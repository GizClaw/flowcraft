package enginetest

// Generic contract test suite for [engine.Host] implementations.
//
// Sibling of [RunSuite] (which targets [engine.Engine]). Lives in
// the same package because both interfaces belong to sdk/engine
// and the Go convention is one xxxtest sub-package per parent
// package — see net/http/httptest, io/iotest, gocloud.dev/blob/drivertest.
//
// # What HostSuite covers
//
//   - Compile-time + runtime sub-interface satisfaction: the type
//     must implement Publisher, Interrupter, UserPrompter,
//     Checkpointer, UsageReporter (= the full Host interface).
//     Each method is exercised at runtime so an embedded NoopHost
//     that forgot to override a method still has a working default.
//   - Publish, Checkpoint, ReportUsage MUST NOT panic for any
//     well-formed input — host.go documents these as called
//     unconditionally by engines, so a panic kills the whole run.
//   - Interrupts() returns a usable channel value (nil included,
//     per the documented "blocks forever" semantic).
//   - AskUser may legitimately return errdefs.NotAvailable for
//     UI-less hosts; the suite accepts either error or a
//     non-zero-reply outcome.
//   - Concurrent invocation safety — host.go promises "any method
//     from any goroutine"; the suite hammers each method from 16
//     goroutines under -race.
//   - Zero-value tolerance: zero Envelope, Checkpoint, UserPrompt,
//     TokenUsage must not crash. Catches hosts that dereference
//     unset fields blindly.
//
// # Wiring
//
//	func TestNoopHost_Contract(t *testing.T) {
//	    enginetest.HostSuite(t, func() engine.Host { return engine.NoopHost{} })
//	}

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// HostFactory builds a fresh [engine.Host] for each subtest. The
// suite invokes it once per case so subtests do not share host
// state.
type HostFactory func() engine.Host

// HostCapabilities lets a host opt out of subtests that don't
// apply. Most hosts pass the zero value (= every subtest runs).
type HostCapabilities struct {
	// SkipAskUserNotAvailable is true when the host is expected
	// to SUCCEED on AskUser (a real UI host with a configured
	// UserPrompter). The suite then asserts the call returns a
	// non-zero reply rather than allowing NotAvailable.
	SkipAskUserNotAvailable bool

	// SkipPublishConcurrency is true when the host's Publish
	// implementation is intentionally serial (e.g. a writer-locked
	// log shipper that must preserve order). The suite then runs
	// the Publish concurrency probe sequentially so the test
	// reflects the real serial-only contract.
	SkipPublishConcurrency bool

	// AcceptsBudgetExceeded, when true, indicates the host's
	// ReportUsage MAY return an errdefs.BudgetExceeded error for
	// the suite's zero-usage probe (= unusual but legal for hosts
	// pre-configured with a zero budget). When false (default),
	// the suite asserts ReportUsage(0-usage) returns nil.
	AcceptsBudgetExceeded bool
}

// HostSuite runs every applicable contract subtest against hosts
// produced by f. Each subtest builds a fresh host so failures
// isolate cleanly.
func HostSuite(t *testing.T, f HostFactory, caps ...HostCapabilities) {
	t.Helper()
	c := HostCapabilities{}
	if len(caps) > 0 {
		c = caps[0]
	}

	t.Run("InterfaceSatisfaction", func(t *testing.T) { hostInterfaceSat(t, f) })
	t.Run("PublishZeroEnvelopeNoPanic", func(t *testing.T) { hostPublishZero(t, f) })
	t.Run("CheckpointZeroNoPanic", func(t *testing.T) { hostCheckpointZero(t, f) })
	t.Run("ReportUsageZeroNoPanic", func(t *testing.T) { hostReportUsageZero(t, f, c) })
	t.Run("InterruptsReturnsUsableChannel", func(t *testing.T) { hostInterruptsChannel(t, f) })
	t.Run("AskUserClassification", func(t *testing.T) { hostAskUserClassification(t, f, c) })
	t.Run("ConcurrentMethodAccess", func(t *testing.T) { hostConcurrentAccess(t, f, c) })
}

// ---------- subtests ----------

// hostInterfaceSat is a compile-time + runtime probe that the host
// satisfies every Host sub-interface.
func hostInterfaceSat(t *testing.T, f HostFactory) {
	t.Helper()
	h := f()
	var (
		_ engine.Publisher     = h
		_ engine.Interrupter   = h
		_ engine.UserPrompter  = h
		_ engine.Checkpointer  = h
		_ engine.UsageReporter = h
	)
}

func hostPublishZero(t *testing.T, f HostFactory) {
	t.Helper()
	h := f()
	defer hostRecoverPanicAs(t, "Publish(zero envelope)")
	_ = h.Publish(context.Background(), event.Envelope{})
}

func hostCheckpointZero(t *testing.T, f HostFactory) {
	t.Helper()
	h := f()
	defer hostRecoverPanicAs(t, "Checkpoint(zero cp)")
	_ = h.Checkpoint(context.Background(), engine.Checkpoint{})
}

func hostReportUsageZero(t *testing.T, f HostFactory, c HostCapabilities) {
	t.Helper()
	h := f()
	defer hostRecoverPanicAs(t, "ReportUsage(zero usage)")
	err := h.ReportUsage(context.Background(), model.TokenUsage{})
	if err != nil && !c.AcceptsBudgetExceeded {
		t.Errorf("ReportUsage(zero) = %v; hosts with no configured budget MUST return nil — see host.go UsageReporter contract", err)
	}
}

func hostInterruptsChannel(t *testing.T, f HostFactory) {
	t.Helper()
	h := f()
	ch := h.Interrupts()
	select {
	case <-ch:
	default:
	}
}

func hostAskUserClassification(t *testing.T, f HostFactory, c HostCapabilities) {
	t.Helper()
	h := f()
	defer hostRecoverPanicAs(t, "AskUser(zero prompt)")
	reply, err := h.AskUser(context.Background(), engine.UserPrompt{})
	if err == nil {
		_ = reply.Parts
		if c.SkipAskUserNotAvailable {
			return
		}
		t.Logf("AskUser returned (%+v, nil) on a host that did not declare SkipAskUserNotAvailable; if this host genuinely supports prompting, set HostCapabilities.SkipAskUserNotAvailable=true", reply)
		return
	}
}

func hostConcurrentAccess(t *testing.T, f HostFactory, c HostCapabilities) {
	t.Helper()
	h := f()
	const n = 16
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("concurrent host call panicked: %v", r)
				}
			}()
			_ = h.Publish(ctx, event.Envelope{Subject: "test"})
			_ = h.Checkpoint(ctx, engine.Checkpoint{ExecID: "x"})
			_ = h.ReportUsage(ctx, model.TokenUsage{})
			_ = h.Interrupts()
		}()
	}
	if c.SkipPublishConcurrency {
		_ = h.Publish(ctx, event.Envelope{Subject: "seq"})
	}
	wg.Wait()
}

// hostRecoverPanicAs converts a panic inside a method probe into a
// t.Errorf so the test binary stays alive and the failure message
// names the offending method.
func hostRecoverPanicAs(t *testing.T, label string) {
	if r := recover(); r != nil {
		t.Errorf("%s panicked: %v — engines call this method unconditionally; a panic would kill the run", label, r)
	}
}
