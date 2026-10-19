package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// stubRunner answers every Start with NotAvailable so approval tests can
// inspect the request without spawning anything.
type stubRunner struct{}

func (stubRunner) Close() error { return nil }

func (stubRunner) Capabilities() sandbox.Capabilities { return sandbox.Capabilities{} }

func (stubRunner) Start(context.Context, sandbox.SessionSpec) (sandbox.Session, error) {
	return nil, errdefs.NotAvailablef("stub runner: no sessions")
}

func (stubRunner) List(context.Context) ([]sandbox.SessionInfo, error) { return nil, nil }

func (stubRunner) Terminate(context.Context, string) error { return nil }

// The workdir predicate decides whether a workdir escape prompts the
// user, so it has to catch a parent traversal and an absolute path
// outside the root while leaving in-root workdirs alone.
func TestWorkDirOutsideRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(filepath.Join(root, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}

	predicate := sandbox.WorkDirOutsideRoot(root)
	for name, tc := range map[string]struct {
		workDir string
		matches bool
	}{
		"root itself":      {root, false},
		"child":            {filepath.Join(root, "child"), false},
		"missing child":    {filepath.Join(root, "not-created"), false},
		"parent traversal": {filepath.Join(root, "..", "outside"), true},
		"absolute outside": {outside, true},
		"relative ignored": {"child", false},
		"empty ignored":    {"", false},
	} {
		reason, matched := predicate.Match(
			sandbox.ExecRequest{Opts: sandbox.ExecOptions{WorkDir: tc.workDir}})
		if matched != tc.matches {
			t.Fatalf("%s: %q matched=%v (%s), want %v",
				name, tc.workDir, matched, reason, tc.matches)
		}
	}
}

// A symlink inside the root that points outside is an escape: the
// workdir has to be resolved, or the prompt is skipped for the very
// case it exists for.
func TestWorkDirOutsideRootFollowsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs privileges on Windows")
	}
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	predicate := sandbox.WorkDirOutsideRoot(root)
	if _, matched := predicate.Match(
		sandbox.ExecRequest{Opts: sandbox.ExecOptions{WorkDir: link}}); !matched {
		t.Fatal("the symlink itself resolves outside and must match")
	}
	if _, matched := predicate.Match(sandbox.ExecRequest{
		Opts: sandbox.ExecOptions{WorkDir: filepath.Join(link, "sub")},
	}); !matched {
		t.Fatal("a workdir below the escaping symlink must match")
	}
}

// TestApprovalSeesDeclaredStdin keeps the approver's view honest: the
// request mirrors the spawn decision (SessionSpec.WantsStdin for Start,
// non-empty bytes for Exec), so a predicate can gate on "this command
// channel carries caller input".
func TestApprovalSeesDeclaredStdin(t *testing.T) {
	var seen []sandbox.ExecRequest
	predicate := sandbox.PredicateFunc(func(req sandbox.ExecRequest) (string, bool) {
		seen = append(seen, req)
		return "", false
	})
	runner := sandbox.WithApproval(stubRunner{}, nil, nil, predicate)
	ctx := context.Background()

	// The stub rejects the spawn; only the request approval saw matters.
	_, _ = runner.Start(ctx, sandbox.SessionSpec{Argv: []string{"true"}, Stdin: true})
	_, _ = sandbox.Exec(ctx, runner, "true", nil,
		sandbox.ExecOptions{Stdin: []byte("x")})
	_, _ = sandbox.Exec(ctx, runner, "true", nil,
		sandbox.ExecOptions{Stdin: []byte{}})

	want := []bool{true, true, false}
	if len(seen) != len(want) {
		t.Fatalf("predicate saw %d requests, want %d", len(seen), len(want))
	}
	for i, req := range seen {
		if req.Stdin != want[i] {
			t.Errorf("request %d: Stdin = %v, want %v", i, req.Stdin, want[i])
		}
	}
}
