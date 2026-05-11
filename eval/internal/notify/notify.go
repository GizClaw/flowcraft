// Package notify delivers eval run events to external IM endpoints.
//
// The package intentionally exposes a tiny Notifier interface (one method)
// so the locomo runner can stay free of any specific provider dependency:
// the runner emits structured events to an [locomo.EventHook]; this package
// supplies adapters that fan those events out.
//
// Backend matrix:
//
//   - [FeishuApp]  — Feishu / Lark application with CardKit. ONE live card
//     per eval run; subsequent events patch the card body
//     in-place. This is the production path.
//   - [Logger]     — dry-run that writes events to stderr instead of
//     talking to any network. Useful for CI smoke tests
//     and local dev without app credentials.
//   - [NoOp]       — silently drop every event.
//
// The Feishu **custom-bot webhook** path is intentionally not supported:
// it posts one chat message per event, which floods the channel on
// long-running evals (LoCoMo10 ≈ 30 min, LongMemEval `_s` ≈ 50 h).
// CardKit cards are the only sane UX at that timescale.
package notify

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

// Event is the unit of communication between the eval runner and a Notifier.
//
// Backends that prefer plain text concatenate Title and Body. Richer
// backends (CardKit cards) can pivot on Kind and read structured payload
// from Fields. Keep Fields values stringly-typed so transport
// serialisation stays trivial.
type Event struct {
	Kind   string            // start | ingest_progress | ingest_done | qa_progress | done | error
	Time   time.Time         // event timestamp, set by the runner
	Title  string            // single-line summary, suitable as a notification subject
	Body   string            // optional multi-line body with details
	Fields map[string]string // optional structured payload for non-text transports
}

// Notifier delivers an [Event] to a downstream channel.
//
// Implementations MUST be safe for concurrent use; the runner may call Notify
// from multiple goroutines (ingest workers, QA workers, the supervising main
// goroutine) without serialisation.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// NoOp drops every event. Returned by [FromFlags] when nothing is
// configured so callers can unconditionally invoke `n.Notify(...)`
// without nil checks.
type NoOp struct{}

// Notify implements [Notifier].
func (NoOp) Notify(context.Context, Event) error { return nil }

// Multi fans an event out to every backend in order. Errors from individual
// backends are logged and swallowed so one broken endpoint does not block
// the rest — notifications are advisory, the eval must keep running.
type Multi []Notifier

// Notify implements [Notifier].
func (m Multi) Notify(ctx context.Context, e Event) error {
	for _, n := range m {
		if n == nil {
			continue
		}
		if err := n.Notify(ctx, e); err != nil {
			log.Printf("[notify] backend %T failed for %s: %v", n, e.Kind, err)
		}
	}
	return nil
}

// FlagOptions is the CLI-facing configuration. Pass it to [FromFlags] to
// build a concrete Notifier; all-empty yields a [NoOp].
type FlagOptions struct {
	// Name is a short human-readable identifier (e.g. "lme-oracle") that
	// gets prepended to every event Title and shown in the CardKit header.
	Name string

	// DryRun routes events to stderr instead of any real endpoint. Used by
	// CI smoke tests and local development without Feishu credentials.
	DryRun bool

	// FeishuAppID / FeishuAppSecret / FeishuChatID enable the CardKit
	// backend: one live-updated card per run. All three must be non-empty;
	// any missing piece falls through to NoOp.
	FeishuAppID     string
	FeishuAppSecret string
	FeishuChatID    string
}

// FromFlags builds a Notifier from CLI / env config. Routing rules:
//
//  1. DryRun                                       → [Logger]
//  2. FeishuAppID + Secret + ChatID all present    → [FeishuApp]
//  3. otherwise                                    → [NoOp]
func FromFlags(opts FlagOptions) (Notifier, error) {
	if opts.DryRun {
		return Logger{Name: opts.Name}, nil
	}
	if opts.FeishuAppID != "" && opts.FeishuAppSecret != "" && opts.FeishuChatID != "" {
		return &FeishuApp{
			AppID:     opts.FeishuAppID,
			AppSecret: opts.FeishuAppSecret,
			ChatID:    opts.FeishuChatID,
			Name:      opts.Name,
		}, nil
	}
	return NoOp{}, nil
}

// Logger writes events to stderr instead of talking to any network. Useful
// for development and `--notify-dry-run` smoke tests so you can see
// exactly what would have been posted without burning rate limit or
// spamming a chat.
type Logger struct {
	Name string
}

// Notify implements [Notifier].
func (l Logger) Notify(_ context.Context, e Event) error {
	title := FormatTitle(l.Name, e.Title)
	if e.Body == "" {
		fmt.Fprintf(stderr(), "[notify dry-run] %s %s\n", e.Kind, title)
	} else {
		fmt.Fprintf(stderr(), "[notify dry-run] %s %s\n%s\n", e.Kind, title, indent(e.Body, "    "))
	}
	return nil
}

// FormatTitle prepends the configured run name to a base title. Used by
// every backend so output looks identical regardless of transport.
func FormatTitle(name, base string) string {
	if name == "" {
		return base
	}
	return fmt.Sprintf("[%s] %s", name, base)
}

// stderr is exposed as a var so tests can capture output. Production
// callers never override it.
var stderr = func() io.Writer { return os.Stderr }

func indent(s, prefix string) string {
	if s == "" {
		return s
	}
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}
