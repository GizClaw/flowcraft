// Package runtime is the top-level orchestrator the `vesseld run`
// CLI invokes. It composes loader → resolver → fleet → api into a
// single Run function with signal handling, token-file loading,
// and structured shutdown.
//
// Keeping every "operational" concern (signals, file IO outside
// configuration, deadline plumbing) in this one place means the
// other packages stay framework-free and easy to test.
package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/cmd/vesseld/api"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/fleet"
	"github.com/GizClaw/flowcraft/cmd/vesseld/loader"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
)

// RunOptions bundles the inputs `vesseld run` collects from CLI
// flags. Ascendancy of the "config bag of paths + flags" pattern
// matches the kubectl-style ergonomics users expect.
type RunOptions struct {
	// Config is one or more --config paths (file or directory).
	Config []string

	// Recursive enables -R style descent for directory inputs.
	Recursive bool

	// Version is the daemon version string surfaced via /v1/version.
	// Defaults to "dev" when empty.
	Version string
}

// Run performs the full daemon lifecycle and returns when the
// process should exit. Returns a non-nil error if startup failed
// or if the api server crashed; nominal SIGTERM exits return nil.
func Run(parent context.Context, opts RunOptions) error {
	objs, err := loader.Load(opts.Config, loader.Options{Recursive: opts.Recursive})
	if err != nil {
		return fmt.Errorf("vesseld run: load config: %w", err)
	}

	cat := catalog.Builtin()
	plan, errs := resolver.Resolve(objs, cat, resolver.DefaultResolveOptions())
	if errs.Len() > 0 {
		return fmt.Errorf("vesseld run: %w", errs.Aggregate())
	}

	logger := buildLogger(plan.Daemon.LoggingFormat, plan.Daemon.LoggingLevel)
	slog.SetDefault(logger)
	logger.Info("vesseld starting",
		"daemon", plan.Daemon.Name,
		"vessels", len(plan.Vessels),
		"socket", plan.Daemon.Socket,
		"listen", plan.Daemon.Listen,
	)

	f, err := fleet.Build(*plan)
	if err != nil {
		return fmt.Errorf("vesseld run: build fleet: %w", err)
	}

	ctx, cancel := signal.NotifyContext(parent, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := f.Launch(ctx); err != nil {
		_ = f.Stop(context.Background())
		return fmt.Errorf("vesseld run: launch fleet: %w", err)
	}

	srv, err := startAPI(ctx, plan, f, opts.Version)
	if err != nil {
		_ = f.Stop(context.Background())
		return fmt.Errorf("vesseld run: start api: %w", err)
	}
	srv.MarkReady()

	logger.Info("vesseld ready", "version", opts.Version)

	// Block until SIGTERM / SIGINT.
	<-ctx.Done()
	logger.Info("vesseld shutdown initiated")

	drainTimeout := plan.Daemon.DrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = 30 * time.Second
	}
	// Each shutdown stage gets its own budget. Sharing a single
	// context across Drain → Stop → API stop meant whichever stage
	// ran first burned the wall clock for everyone after it; if
	// Drain took the whole window the impatient Stop and the API
	// shutdown then ran with a 0-deadline ctx (i.e. immediate
	// cancel), which is the opposite of "graceful".
	//
	// Allocations: Drain gets the configured timeout; Stop gets a
	// shorter budget because by the time we reach Stop, Drain has
	// already given in-flight work its fair shot — Stop's job is
	// just the impatient cancel. API stop gets a final small grace
	// for the HTTP server to close listeners.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
	defer drainCancel()
	if err := f.Drain(drainCtx); err != nil {
		logger.Warn("vesseld drain returned", "err", err)
	}

	stopBudget := drainTimeout / 2
	if stopBudget < 5*time.Second {
		stopBudget = 5 * time.Second
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), stopBudget)
	defer stopCancel()
	if err := f.Stop(stopCtx); err != nil {
		logger.Warn("vesseld stop returned", "err", err)
	}

	apiCtx, apiCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer apiCancel()
	if err := srv.Stop(apiCtx); err != nil {
		logger.Warn("vesseld api stop returned", "err", err)
	}
	logger.Info("vesseld stopped")
	return nil
}

// startAPI builds + starts the API server. Token-file content is
// read here (the only filesystem touch outside loader/secret) so
// the api package stays IO-light and testable.
func startAPI(ctx context.Context, plan *resolver.Plan, f *fleet.Fleet, version string) (*api.Server, error) {
	cfg := api.Config{
		Socket:  plan.Daemon.Socket,
		Listen:  plan.Daemon.Listen,
		Version: version,
	}
	if cfg.Listen != "" {
		raw, err := os.ReadFile(plan.Daemon.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("token file %q: %w", plan.Daemon.TokenFile, err)
		}
		cfg.Token = strings.TrimRight(string(raw), "\r\n")
		if cfg.Token == "" {
			return nil, fmt.Errorf("token file %q is empty", plan.Daemon.TokenFile)
		}
	}
	srv := api.New(cfg, f)
	if err := srv.Start(ctx); err != nil {
		return nil, err
	}
	return srv, nil
}

// buildLogger constructs an slog.Logger honouring the daemon
// document's logging.format / level. Defaults to JSON + INFO so
// production deployments get parseable output without extra config.
func buildLogger(format, level string) *slog.Logger {
	lvl := slog.LevelInfo
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
