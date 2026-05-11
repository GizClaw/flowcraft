package notify

import (
	"context"
	"flag"
	"log"
	"os"
)

// CLIFlags bundles the notification flags shared by every eval CLI
// (locomo/cmd/eval, history/cmd/eval, …). Register it once per binary; the
// returned struct then yields a [Notifier] via [CLIFlags.Build] and a
// canonical [Forward] helper that adapter closures can call without ever
// importing this package's logger.
//
// Credentials are read exclusively from environment variables (FEISHU_APP_ID,
// FEISHU_APP_SECRET, FEISHU_CHAT_ID) to keep secrets out of `ps` listings
// and shell history.
type CLIFlags struct {
	Name        *string
	ProgressPct *int
	DryRun      *bool
}

// RegisterFlags attaches the three notification flags to fs and returns a
// handle for later resolution.
//
//	--notify-name           shown in the card header
//	--notify-progress-pct   milestone resolution (default 25 %)
//	--notify-dry-run        route events to stderr instead of Feishu
func RegisterFlags(fs *flag.FlagSet) *CLIFlags {
	return &CLIFlags{
		Name:        fs.String("notify-name", "", "run identifier shown in the Feishu card header (e.g. lme-oracle); empty disables prefix"),
		ProgressPct: fs.Int("notify-progress-pct", 25, "send milestone notifications every N percent of work (0 disables intermediate updates)"),
		DryRun:      fs.Bool("notify-dry-run", false, "print events to stderr instead of posting to Feishu (for CI / smoke tests)"),
	}
}

// Build resolves the flag values and the FEISHU_* env vars into a concrete
// Notifier. Callers should treat a non-nil error as fatal: a misconfigured
// notifier on a multi-hour run silently drops every milestone, which wastes
// real API budget before anyone notices.
func (c *CLIFlags) Build() (Notifier, error) {
	return FromFlags(FlagOptions{
		Name:            *c.Name,
		DryRun:          *c.DryRun,
		FeishuAppID:     os.Getenv("FEISHU_APP_ID"),
		FeishuAppSecret: os.Getenv("FEISHU_APP_SECRET"),
		FeishuChatID:    os.Getenv("FEISHU_CHAT_ID"),
	})
}

// Forward sends ev through n and logs (rather than returns) the error.
// EventHook adapters call Forward so they can be defined as one-liners:
//
//	opts.Hook = func(ctx context.Context, e history.Event) {
//	    notify.Forward(ctx, notifier, notify.Event{
//	        Kind: e.Kind, Time: e.Time, Title: e.Title, Body: e.Body, Fields: e.Fields,
//	    })
//	}
//
// We deliberately swallow the error: a transient Feishu hiccup must never
// derail an eval run that has already burnt hours of LLM tokens. The
// underlying FeishuApp implementation also retries the token fetch on
// stale-credential errors, so most failures are already self-healing.
func Forward(ctx context.Context, n Notifier, ev Event) {
	if n == nil {
		return
	}
	if err := n.Notify(ctx, ev); err != nil {
		log.Printf("[notify] %s: %v", ev.Kind, err)
	}
}
