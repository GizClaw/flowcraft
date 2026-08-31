//go:build windows

package windows

// Concurrency and scale checks for the Job Object backend: many
// sessions on one runner, several runners at once, concurrent
// AppContainer / WFP isolation sessions, concurrent cleanup, and a
// workspace large enough to make the per-file DACL grants meaningful.
// These run on the elevated windows-latest CI lane; on non-elevated
// hosts the net-policy probes skip as usual.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/sandbox"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
)

// runEcho asserts one-shot exec of `cmd /c echo token` completed with
// exit 0 and exactly the token on stdout — cross-session output
// bleed shows up as an exact-match failure. It returns the error so
// it can be driven from worker goroutines.
func runEcho(r *Runner, token string, opts sandbox.ExecOptions) error {
	res, err := r.Exec(context.Background(), "cmd", []string{"/c", "echo", token}, opts)
	if err != nil {
		return fmt.Errorf("exec %s: %w", token, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exec %s: exit %d", token, res.ExitCode)
	}
	if got := strings.TrimSpace(res.Stdout); got != token {
		return fmt.Errorf("exec %s: stdout %q", token, got)
	}
	return nil
}

// collectErrs drains errs after the workers finish and reports every
// non-nil error through t.
func collectErrs(t *testing.T, errs <-chan error) {
	t.Helper()
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// TestConcurrentSessionsIsolated hammers one runner with parallel
// one-shot sessions and checks that every session gets its own output
// and exit status — the shared SessionRegistry and per-session job
// objects must not cross-talk.
func TestConcurrentSessionsIsolated(t *testing.T) {
	r := mustNewRunner(t)
	const n = 12
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- runEcho(r, fmt.Sprintf("tok-%d", i),
				sandbox.ExecOptions{Timeout: 60 * time.Second})
		}(i)
	}
	wg.Wait()
	close(errs)
	collectErrs(t, errs)
}

// TestConcurrentRunners runs several independent runners at the same
// time, each with a few sequential sessions, so job creation, token
// derivation (if any), and registry bookkeeping race across runners.
func TestConcurrentRunners(t *testing.T) {
	const runners = 4
	const per = 3
	roots := make([]string, runners)
	for i := range roots {
		roots[i] = t.TempDir()
	}
	errs := make(chan error, runners*per)
	var wg sync.WaitGroup
	for i := 0; i < runners; i++ {
		r, err := New(roots[i])
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = r.Close() })
		wg.Add(1)
		go func(i int, r *Runner) {
			defer wg.Done()
			for j := 0; j < per; j++ {
				errs <- runEcho(r, fmt.Sprintf("r%d-%d", i, j),
					sandbox.ExecOptions{Timeout: 60 * time.Second})
			}
		}(i, r)
	}
	wg.Wait()
	close(errs)
	collectErrs(t, errs)
}

// TestConcurrentNetDenyAllSessions creates and tears down several
// AppContainer profiles concurrently, exercising userenv profile
// creation, token derivation, DACL grants, and profile deletion in
// parallel.
func TestConcurrentNetDenyAllSessions(t *testing.T) {
	requireNetIsolation(t)
	r := mustNewRunner(t)
	const n = 5
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- runEcho(r, fmt.Sprintf("deny-%d", i),
				sandbox.ExecOptions{
					Timeout: 90 * time.Second,
					Net:     corenet.NetPolicy{Mode: corenet.NetDenyAll},
				})
		}(i)
	}
	wg.Wait()
	close(errs)
	collectErrs(t, errs)
}

// TestConcurrentNetAllowListSessions stresses the WFP path in
// parallel: every session opens its own engine session, adds its
// sublayer and permit/block filters, starts an enforcement proxy on
// an ephemeral loopback port, and removes everything on close.
func TestConcurrentNetAllowListSessions(t *testing.T) {
	requireNetIsolation(t)
	requireWFP(t)
	r := mustNewRunner(t)
	const n = 3
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- runEcho(r, fmt.Sprintf("allow-%d", i),
				sandbox.ExecOptions{
					Timeout: 90 * time.Second,
					Net: corenet.NetPolicy{
						Mode:       corenet.NetAllowList,
						AllowHosts: []string{"1.1.1.1"},
					},
				})
		}(i)
	}
	wg.Wait()
	close(errs)
	collectErrs(t, errs)
}

// TestConcurrentNetIsolationClose races the teardown path: several
// fully-set-up isolations are closed at once, so WFP cleanup, proxy
// shutdown, profile deletion, and home removal all contend.
func TestConcurrentNetIsolationClose(t *testing.T) {
	requireNetIsolation(t)
	const n = 5
	roots := make([]string, n)
	for i := range roots {
		roots[i] = t.TempDir()
	}
	isos := make([]*netIsolation, n)
	for i := range isos {
		iso, err := newNetIsolation(roots[i], nil,
			corenet.NetPolicy{Mode: corenet.NetDenyAll})
		if err != nil {
			t.Fatalf("newNetIsolation %d: %v", i, err)
		}
		isos[i] = iso
	}
	var wg sync.WaitGroup
	for _, iso := range isos {
		wg.Add(1)
		go func(iso *netIsolation) {
			defer wg.Done()
			if err := iso.Close(); err != nil {
				t.Errorf("concurrent close: %v", err)
			}
		}(iso)
	}
	wg.Wait()
}

// TestLargeWorkspaceGrantScales builds a workspace with hundreds of
// files and runs one NetDenyAll session over it. The per-file DACL
// grants (GetNamedSecurityInfo / SetEntriesInAclW /
// SetNamedSecurityInfo per node) must complete in a bounded time
// instead of degrading quadratically.
func TestLargeWorkspaceGrantScales(t *testing.T) {
	requireNetIsolation(t)
	root := t.TempDir()
	const dirs = 8
	const filesPerDir = 50
	for d := 0; d < dirs; d++ {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		for f := 0; f < filesPerDir; f++ {
			p := filepath.Join(dir, fmt.Sprintf("f%03d.txt", f))
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatalf("write %s: %v", p, err)
			}
		}
	}
	r, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = r.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := r.Exec(ctx, "cmd", []string{"/c", "echo", "ok"},
		sandbox.ExecOptions{
			Timeout: 110 * time.Second,
			Net:     corenet.NetPolicy{Mode: corenet.NetDenyAll},
		})
	if err != nil {
		t.Fatalf("exec over %d-file workspace: %v", dirs*filesPerDir, err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "ok" {
		t.Fatalf("exit=%d stdout=%q, want ok", res.ExitCode, res.Stdout)
	}
}
