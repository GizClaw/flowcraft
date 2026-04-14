package workspace

import (
	"context"
	"sync"
	"testing"
)

func TestScopedWorkspace_ReadWrite(t *testing.T) {
	inner := NewMemWorkspace()
	inner.MustWrite("allowed/file.txt", []byte("ok"))
	inner.MustWrite("secret/key.txt", []byte("secret"))

	sw := NewScopedWorkspace(inner,
		WithAllowWrite("allowed/**"),
		WithDenyRead("secret/**"),
	)
	ctx := context.Background()

	if _, err := sw.Read(ctx, "allowed/file.txt"); err != nil {
		t.Fatalf("read allowed: %v", err)
	}
	if _, err := sw.Read(ctx, "secret/key.txt"); err == nil {
		t.Fatal("expected read denied")
	}
	if err := sw.Write(ctx, "allowed/new.txt", []byte("new")); err != nil {
		t.Fatalf("write allowed: %v", err)
	}
	if err := sw.Write(ctx, "other/file.txt", []byte("no")); err == nil {
		t.Fatal("expected write denied")
	}
}

func TestScopedWorkspace_AllOps(t *testing.T) {
	inner := NewMemWorkspace()
	inner.MustWrite("zone/data.txt", []byte("data"))
	inner.MustWrite("zone/sub/deep.txt", []byte("deep"))

	sw := NewScopedWorkspace(inner,
		WithAllowWrite("zone/**"),
	)
	ctx := context.Background()

	if err := sw.Append(ctx, "zone/data.txt", []byte(" more")); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, _ := inner.Read(ctx, "zone/data.txt")
	if string(data) != "data more" {
		t.Fatalf("got %q", data)
	}

	if _, err := sw.List(ctx, "zone"); err != nil {
		t.Fatalf("list: %v", err)
	}

	if _, err := sw.Exists(ctx, "zone/data.txt"); err != nil {
		t.Fatalf("exists: %v", err)
	}

	if _, err := sw.Stat(ctx, "zone/data.txt"); err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err := sw.Delete(ctx, "zone/data.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if err := sw.RemoveAll(ctx, "zone/sub"); err != nil {
		t.Fatalf("removeall: %v", err)
	}
}

func TestScopedWorkspace_AllOpsDenied(t *testing.T) {
	inner := NewMemWorkspace()
	inner.MustWrite("readonly/f.txt", []byte("x"))
	inner.MustWrite("hidden/f.txt", []byte("x"))

	sw := NewScopedWorkspace(inner,
		WithDenyRead("hidden/**"),
	)
	ctx := context.Background()

	if err := sw.Append(ctx, "readonly/f.txt", []byte("x")); err == nil {
		t.Fatal("append should be denied (no allowWrite)")
	}
	if err := sw.Delete(ctx, "readonly/f.txt"); err == nil {
		t.Fatal("delete should be denied")
	}
	if err := sw.RemoveAll(ctx, "readonly"); err == nil {
		t.Fatal("removeall should be denied")
	}

	if _, err := sw.List(ctx, "hidden"); err == nil {
		t.Fatal("list hidden should be denied")
	}
	if _, err := sw.Exists(ctx, "hidden/f.txt"); err == nil {
		t.Fatal("exists hidden should be denied")
	}
	if _, err := sw.Stat(ctx, "hidden/f.txt"); err == nil {
		t.Fatal("stat hidden should be denied")
	}
}

func TestScopedWorkspace_MandatoryDeny(t *testing.T) {
	inner := NewMemWorkspace()
	inner.MustWrite("creds/token.json", []byte("secret"))

	sw := NewScopedWorkspace(inner,
		WithAllowWrite("creds/**"),
		WithMandatoryDeny("creds/**"),
	)
	ctx := context.Background()

	if _, err := sw.Read(ctx, "creds/token.json"); err == nil {
		t.Fatal("mandatory deny should block read even when not in denyRead")
	}
	if err := sw.Write(ctx, "creds/new.json", []byte("x")); err == nil {
		t.Fatal("mandatory deny should block write even when in allowWrite")
	}
}

func TestScopedWorkspace_CustomLogger(t *testing.T) {
	var mu sync.Mutex
	var violations []ViolationRecord

	logger := &testViolationLogger{
		fn: func(_ context.Context, r ViolationRecord) {
			mu.Lock()
			violations = append(violations, r)
			mu.Unlock()
		},
	}

	inner := NewMemWorkspace()
	inner.MustWrite("secret/key.txt", []byte("x"))

	sw := NewScopedWorkspace(inner,
		WithDenyRead("secret/**"),
		WithViolationLogger(logger),
	)

	sw.Read(context.Background(), "secret/key.txt")

	mu.Lock()
	defer mu.Unlock()
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(violations))
	}
	if violations[0].Operation != "read" {
		t.Fatalf("operation = %q, want 'read'", violations[0].Operation)
	}
}

type testViolationLogger struct {
	fn func(context.Context, ViolationRecord)
}

func (l *testViolationLogger) LogViolation(ctx context.Context, r ViolationRecord) {
	l.fn(ctx, r)
}

func TestScopedGitWorkspace(t *testing.T) {
	inner := NewMemWorkspace()

	gw := &fakeGitWorkspace{Workspace: inner}

	sgw := NewScopedGitWorkspace(gw,
		WithAllowWrite("repos/**"),
		WithDenyRead("private/**"),
	)
	ctx := context.Background()

	if err := sgw.GitClone(ctx, "https://example.com/repo", "repos/myrepo"); err != nil {
		t.Fatalf("clone allowed: %v", err)
	}
	if err := sgw.GitClone(ctx, "https://example.com/repo", "other/repo"); err == nil {
		t.Fatal("clone should be denied outside allowWrite")
	}

	if err := sgw.GitPull(ctx, "repos/myrepo"); err != nil {
		t.Fatalf("pull allowed: %v", err)
	}
	if err := sgw.GitPull(ctx, "other/repo"); err == nil {
		t.Fatal("pull should be denied")
	}

	inner.MustWrite("repos/myrepo/.git/HEAD", []byte("x"))
	if _, err := sgw.GitHead(ctx, "repos/myrepo"); err != nil {
		t.Fatalf("head allowed: %v", err)
	}
	if _, err := sgw.GitHead(ctx, "private/repo"); err == nil {
		t.Fatal("head should be denied for denyRead")
	}
}

type fakeGitWorkspace struct {
	Workspace
}

func (f *fakeGitWorkspace) GitClone(_ context.Context, _, _ string) error { return nil }
func (f *fakeGitWorkspace) GitPull(_ context.Context, _ string) error     { return nil }
func (f *fakeGitWorkspace) GitHead(_ context.Context, _ string) (string, error) {
	return "abc1234", nil
}

func TestMatchesAny(t *testing.T) {
	tests := []struct {
		path    string
		pattern string
		want    bool
	}{
		// dir/** prefix
		{"secret/key.txt", "secret/**", true},
		{"secret", "secret/**", true},
		{"secret/a/b/c", "secret/**", true},
		{"secrets/key.txt", "secret/**", false},

		// **/dir/** anywhere
		{"a/b/secret/key.txt", "**/secret/**", true},
		{"secret/key.txt", "**/secret/**", true},
		{"secret", "**/secret/**", true},
		{"a/secret", "**/secret/**", true},

		// **/name exact at any depth
		{"config.yaml", "**/config.yaml", true},
		{"a/b/config.yaml", "**/config.yaml", true},
		{"config.yaml.bak", "**/config.yaml", false},

		// **/glob at any depth
		{"a/b/key.pem", "**/*.pem", true},
		{"key.pem", "**/*.pem", true},
		{"a/key.pem.bak", "**/*.pem", false},

		// Without ** prefix, *.ext only matches at top level
		{"key.pem", "*.pem", true},
		{"a/key.pem", "*.pem", false},

		// exact match
		{"foo.txt", "foo.txt", true},
		{"dir/foo.txt", "foo.txt", false},

		// empty patterns
		{"any", "", false},
	}
	for _, tt := range tests {
		patterns := []string{tt.pattern}
		if tt.pattern == "" {
			patterns = nil
		}
		got := matchesAny(tt.path, patterns)
		if got != tt.want {
			t.Errorf("matchesAny(%q, [%q]) = %v, want %v", tt.path, tt.pattern, got, tt.want)
		}
	}
}

func TestMatchesAny_NoPatterns(t *testing.T) {
	if matchesAny("any/path", nil) {
		t.Fatal("nil patterns should never match")
	}
	if matchesAny("any/path", []string{}) {
		t.Fatal("empty patterns should never match")
	}
}
