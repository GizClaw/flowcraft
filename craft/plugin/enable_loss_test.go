package plugin

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// The tests here cover the seam between an enable's wait and the loss
// report: a give-up that arrives while a call is still deciding what its
// plugin is. With a scripted [Readiness] the loss is fired from inside
// the wait, which is the window no source can otherwise reach on demand.

// TestHostEnableReportsLossHeldByTheWait covers a terminal loss that
// lands while an enable is waiting. The call in flight is what narrates
// its plugin's failure, so the loss is held for it to decide about: when
// the enable stands, the application still has to hear that the plugin
// lost a server — otherwise the entry is recorded enabled with a dead
// process behind it and nothing is reported at all.
func TestHostEnableReportsLossHeldByTheWait(t *testing.T) {
	t.Parallel()
	factory := newReadyFactory(nil)
	host, store, events, _ := startReadyHost(t, HostOptions{}, factory)
	factory.answerWait(func(context.Context, time.Duration) error {
		host.notifyLost("hello")
		return nil
	})

	if err := host.SetEnabled(context.Background(), "hello", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !enabled(t, store, "hello") {
		t.Fatal("the entry was not recorded enabled")
	}
	if got := events.snapshot(); !slices.Equal(got, []string{"hello:failed"}) {
		t.Fatalf("stop events = %v, want [hello:failed]", got)
	}
}

// TestHostEnableReportsLossAfterAnUnverifiedWait covers what the
// per-server wait reaches in practice: one server gives up while the call
// is still waiting on another, and the call then ends without a verdict —
// the plugin is recorded enabled and its source keeps reconnecting. The
// loss the wait collected is still one the application has to hear, or
// the entry says enabled about a plugin one of whose servers has no
// process and no report.
func TestHostEnableReportsLossAfterAnUnverifiedWait(t *testing.T) {
	t.Parallel()
	bound := 20 * time.Millisecond
	factory := newReadyFactory(nil)
	host, store, events, _ := startReadyHost(t, HostOptions{EnableTimeout: bound}, factory)
	factory.answerWait(func(ctx context.Context, _ time.Duration) error {
		host.notifyLost("hello")
		<-ctx.Done()
		return ctx.Err()
	})

	if err := host.SetEnabled(context.Background(), "hello", true); err != nil {
		t.Fatalf("enable of a plugin that is not up yet: %v", err)
	}
	if !enabled(t, store, "hello") {
		t.Fatal("the entry was not recorded enabled")
	}
	if !running(host, "hello") {
		t.Fatal("the source was dropped instead of left reconnecting")
	}
	if got := events.snapshot(); !slices.Equal(got, []string{"hello:failed"}) {
		t.Fatalf("stop events = %v, want [hello:failed]", got)
	}
}

// TestHostEnableRollbackNarratesTheLoss pins the other side of the same
// decision: a loss the wait collected is not reported separately when the
// enable rolls back, because the rollback is that failure's report.
func TestHostEnableRollbackNarratesTheLoss(t *testing.T) {
	t.Parallel()
	factory := newReadyFactory(nil)
	host, store, events, _ := startReadyHost(t, HostOptions{}, factory)
	factory.answerWait(func(context.Context, time.Duration) error {
		host.notifyLost("hello")
		return errdefs.Validationf(
			"plugin hello: mcp: server hello: cannot start command: gone")
	})

	err := host.SetEnabled(context.Background(), "hello", true)
	if !errdefs.IsValidation(err) {
		t.Fatalf("enable = %v, want the source's terminal failure", err)
	}
	if enabled(t, store, "hello") {
		t.Fatal("a rolled-back enable was recorded as enabled")
	}
	if got := events.snapshot(); !slices.Equal(got, []string{"hello:rolled_back"}) {
		t.Fatalf("stop events = %v, want [hello:rolled_back]", got)
	}
}

// TestHostEnableReplacesASourceThatGaveUp covers the other end of the
// loss report: re-enabling is the natural reaction to it, so it has to
// mean something. The source that gave up is retired and a new one
// started, so the call reports the truth about the plugin instead of
// finding the dead one "already running".
func TestHostEnableReplacesASourceThatGaveUp(t *testing.T) {
	t.Parallel()
	factory := newReadyFactory(nil)
	host, _, events, hook := startReadyHost(t, HostOptions{}, factory)
	ctx := context.Background()
	if err := host.SetEnabled(ctx, "hello", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// The plugin's process gives up on its own: the entry keeps its state
	// and the loss is what the application is told.
	host.notifyLost("hello")
	if got := events.snapshot(); !slices.Equal(got, []string{"hello:failed"}) {
		t.Fatalf("stop events = %v, want [hello:failed]", got)
	}

	if err := host.SetEnabled(ctx, "hello", true); err != nil {
		t.Fatalf("enable after a loss: %v", err)
	}
	if got := factory.closes("hello"); got != 1 {
		t.Fatalf("the dead source was closed %d times, want 1", got)
	}
	if got := factory.waits(); len(got) != 2 {
		t.Fatalf("readiness waits = %v, want one per enable", got)
	}
	if !running(host, "hello") {
		t.Fatal("the replacement source is not recorded")
	}
	if got := toolNames(host); !slices.Contains(got, "hello__echo") {
		t.Fatalf("tools = %v, want the replacement's", got)
	}
	// Replacing a source is not a second report of the same loss.
	if got := events.snapshot(); !slices.Equal(got, []string{"hello:failed"}) {
		t.Fatalf("stop events = %v, want [hello:failed]", got)
	}
	// The internal hook still runs for the source being retired, so the
	// token minted for the dead process is revoked before the
	// replacement mints its own.
	if got := hook.count(); got != 1 {
		t.Fatalf("the internal stopped hook ran %d times, want 1", got)
	}
}

// TestHostEnableRollsBackARetryAfterALoss covers what re-enabling a
// plugin that gave up does when the plugin cannot come up either: the
// attempt is reported — both as the error the caller sees and as the stop
// it did not ask for — and the record stops saying enabled about a plugin
// whose process is gone.
func TestHostEnableRollsBackARetryAfterALoss(t *testing.T) {
	t.Parallel()
	factory := newReadyFactory(nil)
	host, store, events, _ := startReadyHost(t, HostOptions{}, factory)
	ctx := context.Background()
	if err := host.SetEnabled(ctx, "hello", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	host.notifyLost("hello")
	factory.answerWait(func(context.Context, time.Duration) error {
		return errdefs.Validationf(
			"plugin hello: mcp: server hello: cannot start command: still gone")
	})

	err := host.SetEnabled(ctx, "hello", true)
	if !errdefs.IsValidation(err) {
		t.Fatalf("retry = %v, want the source's terminal failure", err)
	}
	if enabled(t, store, "hello") {
		t.Fatal("the record still says enabled about a plugin with no process")
	}
	if running(host, "hello") {
		t.Fatal("the rolled-back source is still recorded")
	}
	if got := events.snapshot(); !slices.Equal(got,
		[]string{"hello:failed", "hello:rolled_back"}) {
		t.Fatalf("stop events = %v, want [hello:failed hello:rolled_back]", got)
	}
}
